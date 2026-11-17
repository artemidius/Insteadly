package mistral

import (
	"encoding/binary"
	"math"
	"testing"
)

func float32LE(v float32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	return b
}

func TestDecodeFloat32LE(t *testing.T) {
	in := append(float32LE(-1), append(float32LE(0), float32LE(1)...)...)
	got, err := decodeFloat32LE(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 || got[0] != -1 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("decoded = %v", got)
	}
	if _, err := decodeFloat32LE(make([]byte, 3)); err == nil {
		t.Error("non-multiple-of-4 must error")
	}
	if _, err := decodeFloat32LE(float32LE(float32(math.NaN()))); err == nil {
		t.Error("NaN must error")
	}
	if _, err := decodeFloat32LE(float32LE(float32(math.Inf(1)))); err == nil {
		t.Error("Inf must error")
	}
}

func TestFloatToInt16(t *testing.T) {
	cases := []struct {
		in   float64
		want int16
	}{
		{-1, -32768},
		{1, 32767},
		{0, 0},
		{0.5, 16384},
		{-0.5, -16384},
		{-2, -32768},
		{3, 32767},
	}
	for _, tc := range cases {
		if got := floatToInt16(tc.in); got != tc.want {
			t.Errorf("floatToInt16(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResamplerCoeffsNormalizedAndSymmetric(t *testing.T) {
	c := resamplerCoeffs()
	var sum float64
	for _, v := range c {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("coefficients sum = %v, want 1", sum)
	}
	for n := 0; n < taps; n++ {
		if math.Abs(c[n]-c[taps-1-n]) > 1e-9 {
			t.Errorf("coefficient %d not symmetric with %d", n, taps-1-n)
		}
	}
}

func TestResamplerImpulseGolden(t *testing.T) {
	// A single impulse at input position 0 lets us verify the polyphase center and
	// half-sample interpolation against closed-form expectations.
	c := resamplerCoeffs()
	r := newResampler()
	input := make([]float64, 8)
	input[0] = 1
	got := append(r.feed(input), r.finish()...)

	want := []int16{
		floatToInt16(c[7]),
		floatToInt16((c[6] + c[5]) / 2),
		floatToInt16(c[4]),
		floatToInt16((c[3] + c[2]) / 2),
	}
	if len(got) < len(want) {
		t.Fatalf("outputs = %d, want at least %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("output[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestResampler480To320(t *testing.T) {
	r := newResampler()
	input := make([]float64, 480)
	for i := range input {
		input[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / ttsInputRate)
	}
	out := append(r.feed(input), r.finish()...)
	if len(out) != 320 {
		t.Fatalf("output samples = %d, want 320", len(out))
	}
}

func TestResamplerSplitBoundaries(t *testing.T) {
	n := 480
	whole := make([]float64, n)
	for i := range whole {
		whole[i] = math.Sin(2 * math.Pi * 440 * float64(i) / ttsInputRate)
	}
	r1 := newResampler()
	want := append(r1.feed(whole), r1.finish()...)

	r2 := newResampler()
	var got []int16
	// Feed in awkward chunk sizes including 1-sample chunks.
	sizes := []int{1, 3, 5, 7, 13, 51, 400}
	idx := 0
	for _, size := range sizes {
		if idx >= n {
			break
		}
		end := idx + size
		if end > n {
			end = n
		}
		got = append(got, r2.feed(whole[idx:end])...)
		idx = end
	}
	if idx < n {
		got = append(got, r2.feed(whole[idx:])...)
	}
	got = append(got, r2.finish()...)

	if len(got) != len(want) {
		t.Fatalf("split output %d != whole %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("output[%d] = %d, whole = %d (phase/boundary error)", i, got[i], want[i])
		}
	}
}

func TestResamplerAttenuates10kHz(t *testing.T) {
	const seconds = 0.5
	n := int(seconds * ttsInputRate)
	mk := func(freq float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = math.Sin(2 * math.Pi * freq * float64(i) / ttsInputRate)
		}
		return out
	}
	rms := func(s []int16) float64 {
		var sum float64
		for _, v := range s {
			sum += float64(v) * float64(v)
		}
		return math.Sqrt(sum / float64(len(s)))
	}

	rLow := newResampler()
	lowOut := append(rLow.feed(mk(1000)), rLow.finish()...)
	rHigh := newResampler()
	highOut := append(rHigh.feed(mk(10000)), rHigh.finish()...)

	if rms(lowOut) < 1000 {
		t.Fatalf("1 kHz tone too attenuated: rms=%v", rms(lowOut))
	}
	if rms(highOut) > 0.05*rms(lowOut) {
		t.Errorf("10 kHz tone not sufficiently attenuated: rms=%v vs 1 kHz rms=%v", rms(highOut), rms(lowOut))
	}
}

func TestResamplerBoundedState(t *testing.T) {
	r := newResampler()
	input := make([]float64, ttsInputRate) // 1 second
	for i := range input {
		input[i] = math.Sin(2 * math.Pi * 440 * float64(i) / ttsInputRate)
	}
	r.feed(input)
	r.finish()
	if len(r.hist) >= 64 {
		t.Errorf("history not bounded: %d samples retained", len(r.hist))
	}
}

func TestFramer(t *testing.T) {
	f := &framer{}
	frames := f.push(make([]byte, 640))
	if len(frames) != 1 || len(frames[0]) != 640 {
		t.Fatalf("frames = %d", len(frames))
	}
	if got := f.flush(); got != nil {
		t.Error("flush with no residual must be nil")
	}

	f2 := &framer{}
	var got [][]byte
	for i := 0; i < 7; i++ {
		got = append(got, f2.push(make([]byte, 100))...)
	}
	if len(got) != 1 || len(got[0]) != 640 {
		t.Fatalf("700 bytes should yield one full frame, got %d", len(got))
	}
	pad := f2.flush()
	if len(pad) != 640 {
		t.Fatalf("padded frame = %d bytes, want 640", len(pad))
	}
	if got := f2.flush(); got != nil {
		t.Error("second flush must be nil")
	}
}
