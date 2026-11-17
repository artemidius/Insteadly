package telnyx

import (
	"encoding/binary"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Telnyx media streams carry a G.711 µ-law, mono, 8 kHz RTP payload. Inside the adapter
// every byte of that payload is converted to the core's canonical PCM16 LE mono 16 kHz,
// and back. This is a self-contained copy of the conversion already proven in the Twilio
// adapter: adapters do not import each other and the core must never depend on codecs.
const (
	// telnyxFrameBytes is one 20 ms Telnyx frame: 160 µ-law bytes at 8 kHz.
	telnyxFrameBytes = 160
	// It decodes to 320 samples of PCM16 = domain.FrameBytes (640 bytes).
)

// muLawDecodeTable maps all 256 G.711 µ-law codes to PCM16 samples.
var muLawDecodeTable = func() [256]int16 {
	var table [256]int16
	for i := range table {
		table[i] = muLawToPCM16(byte(i))
	}
	return table
}()

// decodeMuLaw converts one G.711 µ-law byte to a PCM16 sample.
func decodeMuLaw(b byte) int16 { return muLawDecodeTable[b] }

func muLawToPCM16(b byte) int16 {
	u := ^b
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0f
	magnitude := (int32(mantissa)<<3 + 0x84) << exponent
	magnitude -= 0x84
	if sign != 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}

// encodeMuLaw converts one PCM16 sample to G.711 µ-law. It is the standard encoder
// (bias 0x84, clip 32635, segment search), so encode(decode(b)) == b for every code
// except the non-canonical positive zero 0x7f, which canonicalizes to 0xff.
func encodeMuLaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	v := int32(sample)
	var sign byte
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > clip {
		v = clip
	}
	v += bias

	exponent := 7
	for mask := int32(0x4000); exponent > 0 && v&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte((v >> (exponent + 3)) & 0x0f)
	return ^(sign | byte(exponent<<4) | mantissa)
}

// upsample8to16 doubles the sample rate by linear interpolation. The output has exactly
// twice as many samples as the input.
func upsample8to16(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int16, len(in)*2)
	for i, s := range in {
		out[2*i] = s
		next := s
		if i+1 < len(in) {
			next = in[i+1]
		}
		out[2*i+1] = int16((int32(s) + int32(next)) / 2)
	}
	return out
}

// lowPass16k is a 9-tap Hamming-windowed sinc low-pass at ~3.4 kHz for 16 kHz input,
// scaled by 32768. It removes the energy that would alias when decimating to 8 kHz.
var lowPass16k = [9]int32{-170, -571, 1287, 8831, 14015, 8831, 1287, -571, -170}

// downsample16to8 applies the anti-alias filter and keeps every second sample.
func downsample16to8(in []int16) []int16 {
	if len(in) < 2 {
		return nil
	}
	out := make([]int16, len(in)/2)
	for j := range out {
		center := 2 * j
		var acc int64
		for k, c := range lowPass16k {
			idx := center + k - 4
			if idx < 0 {
				idx = 0
			} else if idx >= len(in) {
				idx = len(in) - 1
			}
			acc += int64(in[idx]) * int64(c)
		}
		sample := acc >> 15
		if sample > 32767 {
			sample = 32767
		} else if sample < -32768 {
			sample = -32768
		}
		out[j] = int16(sample)
	}
	return out
}

// frameEncoder turns arbitrary canonical PCM16 chunks into whole 20 ms µ-law frames.
// Residual bytes are kept for the next chunk; a partial frame at call end is dropped.
type frameEncoder struct {
	pcm []byte
}

// encode consumes canonical PCM16 LE bytes and returns zero or more 160-byte µ-law
// frames (one per 20 ms).
func (e *frameEncoder) encode(pcm []byte) [][]byte {
	if len(pcm) == 0 {
		return nil
	}
	e.pcm = append(e.pcm, pcm...)
	frames := make([][]byte, 0, len(e.pcm)/domain.FrameBytes)
	for len(e.pcm) >= domain.FrameBytes {
		samples := make([]int16, domain.FrameBytes/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(e.pcm[2*i:]))
		}
		resampled := downsample16to8(samples)
		frame := make([]byte, len(resampled))
		for i, s := range resampled {
			frame[i] = encodeMuLaw(s)
		}
		frames = append(frames, frame)
		e.pcm = e.pcm[domain.FrameBytes:]
	}
	if len(e.pcm) == 0 {
		e.pcm = nil
	}
	return frames
}

// frameDecoder turns arbitrary µ-law chunks into whole 20 ms canonical PCM16 chunks.
// Residual samples are kept for the next chunk.
type frameDecoder struct {
	muLaw []byte
}

// decode consumes µ-law bytes and returns zero or more 640-byte canonical chunks.
func (d *frameDecoder) decode(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	d.muLaw = append(d.muLaw, payload...)
	chunks := make([][]byte, 0, len(d.muLaw)/telnyxFrameBytes)
	for len(d.muLaw) >= telnyxFrameBytes {
		raw := d.muLaw[:telnyxFrameBytes]
		samples := make([]int16, len(raw))
		for i, b := range raw {
			samples[i] = decodeMuLaw(b)
		}
		upsampled := upsample8to16(samples)
		chunk := make([]byte, len(upsampled)*2)
		for i, s := range upsampled {
			binary.LittleEndian.PutUint16(chunk[2*i:], uint16(s))
		}
		chunks = append(chunks, chunk)
		d.muLaw = d.muLaw[telnyxFrameBytes:]
	}
	if len(d.muLaw) == 0 {
		d.muLaw = nil
	}
	return chunks
}
