package telnyx

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestMuLawKnownValues(t *testing.T) {
	cases := map[byte]int16{
		0x00: -32124,
		0x7f: 0,
		0x80: 32124,
		0xff: 0,
	}
	for in, want := range cases {
		if got := decodeMuLaw(in); got != want {
			t.Errorf("decodeMuLaw(0x%02x) = %d, want %d", in, got, want)
		}
	}
}

func TestMuLawSampleRoundTrip(t *testing.T) {
	for code := 0; code < 256; code++ {
		sample := decodeMuLaw(byte(code))
		if got := decodeMuLaw(encodeMuLaw(sample)); got != sample {
			t.Fatalf("sample round trip failed for code 0x%02x: %d -> %d", code, sample, got)
		}
		if code != 0x7f {
			if got := encodeMuLaw(sample); got != byte(code) {
				t.Fatalf("encode(decode(0x%02x)) = 0x%02x", code, got)
			}
		}
	}
}

func TestUpsample8to16(t *testing.T) {
	in := []int16{0, 100, -100}
	out := upsample8to16(in)
	if len(out) != len(in)*2 {
		t.Fatalf("len = %d, want %d", len(out), len(in)*2)
	}
	if out[0] != 0 || out[2] != 100 || out[4] != -100 {
		t.Errorf("original samples not preserved: %v", out)
	}
	if out[1] != 50 || out[5] != -100 {
		t.Errorf("midpoints = %v", out)
	}
}

func sine16k(freq float64, samples int) []int16 {
	out := make([]int16, samples)
	for i := range out {
		out[i] = int16(20000 * math.Sin(2*math.Pi*freq*float64(i)/16000))
	}
	return out
}

func rms(in []int16) float64 {
	if len(in) == 0 {
		return 0
	}
	var sum float64
	for _, s := range in {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(in)))
}

func TestDownsample16to8(t *testing.T) {
	in := sine16k(1000, 3200)
	out := downsample16to8(in)
	if len(out) != len(in)/2 {
		t.Fatalf("len = %d, want %d", len(out), len(in)/2)
	}
	low := rms(downsample16to8(sine16k(1000, 3200)))
	high := rms(downsample16to8(sine16k(6000, 3200)))
	if low == 0 {
		t.Fatal("1 kHz reference is silent")
	}
	if ratio := high / low; ratio > 0.2 {
		t.Errorf("6 kHz/1 kHz RMS ratio = %.3f, want <= 0.2 (anti-alias filter)", ratio)
	}
}

func TestFrameEncoderDecoderRoundTrip(t *testing.T) {
	const samples = 1600 // 100 ms at 16 kHz
	in := sine16k(440, samples)
	pcm := make([]byte, len(in)*2)
	for i, s := range in {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(s))
	}

	var enc frameEncoder
	var frames [][]byte
	for off := 0; off < len(pcm); {
		n := 637
		if off+n > len(pcm) {
			n = len(pcm) - off
		}
		frames = append(frames, enc.encode(pcm[off:off+n])...)
		off += n
	}
	if len(frames) != 5 {
		t.Fatalf("frames = %d, want 5 (100 ms / 20 ms)", len(frames))
	}
	for i, f := range frames {
		if len(f) != telnyxFrameBytes {
			t.Fatalf("frame %d length = %d, want %d", i, len(f), telnyxFrameBytes)
		}
	}

	var dec frameDecoder
	var chunks [][]byte
	for _, f := range frames {
		chunks = append(chunks, dec.decode(f)...)
	}
	if len(chunks) != 5 {
		t.Fatalf("chunks = %d, want 5", len(chunks))
	}
	for i, c := range chunks {
		if len(c) != domain.FrameBytes {
			t.Fatalf("chunk %d length = %d, want %d", i, len(c), domain.FrameBytes)
		}
	}
}
