package mistral

import (
	"encoding/binary"
	"errors"
	"math"
)

// Canonical audio constants for the conversion pipeline (Mistral TTS emits float32 LE
// mono 24 kHz; the core wants int16 LE mono 16 kHz).
const (
	ttsInputRate  = 24000
	ttsOutputRate = 16000
	taps          = 15
	tapCenter     = 7
)

// decodeFloat32LE decodes a raw float32 little-endian mono buffer and rejects NaN/Inf.
func decodeFloat32LE(b []byte) ([]float64, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("mistral tts: audio delta length is not a multiple of 4")
	}
	out := make([]float64, len(b)/4)
	for i := range out {
		f := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, errors.New("mistral tts: audio contains NaN or Inf")
		}
		out[i] = float64(f)
	}
	return out, nil
}

// resampler converts float32 mono 24 kHz samples to int16 mono 16 kHz using a 15-tap
// Hamming-windowed sinc low-pass (cutoff 7.2 kHz) and 3:2 polyphase decimation with linear
// interpolation at half-sample phases. History and phase persist across arbitrary deltas.
type resampler struct {
	coeff []float64
	hist  []float64 // input samples [base .. base+len-1]
	base  int       // absolute index of hist[0]
	next  int       // next output sample index
	total int       // real input samples fed
	done  bool
}

func newResampler() *resampler {
	return &resampler{coeff: resamplerCoeffs()}
}

// resamplerCoeffs computes the normalized 15-tap low-pass coefficients.
func resamplerCoeffs() []float64 {
	const f = 7200.0 / 24000.0 // 0.3
	coeff := make([]float64, taps)
	var sum float64
	for n := 0; n < taps; n++ {
		x := 2 * f * float64(n-tapCenter)
		sinc := 1.0
		if x != 0 {
			sinc = math.Sin(math.Pi*x) / (math.Pi * x)
		}
		h := 2 * f * sinc * (0.54 - 0.46*math.Cos(2*math.Pi*float64(n)/float64(taps-1)))
		coeff[n] = h
		sum += h
	}
	for n := range coeff {
		coeff[n] /= sum
	}
	return coeff
}

// feed adds input samples and returns any newly producible int16 output samples.
func (r *resampler) feed(samples []float64) []int16 {
	r.hist = append(r.hist, samples...)
	r.total += len(samples)
	return r.produce()
}

// finish flushes the remaining outputs whose position is inside the original input
// timeline (< total). Future samples beyond total are zero (tail padding). Idempotent.
func (r *resampler) finish() []int16 {
	if r.done {
		return nil
	}
	r.done = true
	return r.produce()
}

// produce emits every output whose position is inside the input timeline and whose needed
// input samples are available (or tail-padded once done).
func (r *resampler) produce() []int16 {
	var out []int16
	for r.canProduce() {
		out = append(out, floatToInt16(r.compute(r.next)))
		r.next++
		r.advance()
	}
	return out
}

// canProduce reports whether output r.next is ready: its position is inside the timeline
// and enough real input has arrived (or we are done and tail samples are zero).
func (r *resampler) canProduce() bool {
	p := 1.5 * float64(r.next)
	if r.done {
		return p < float64(r.total)
	}
	need := int(math.Ceil(p)) + tapCenter + 1 // ceil(1.5m) + 8
	return r.total >= need
}

// compute evaluates output m at input position 1.5m, interpolating adjacent filtered
// integer positions for half-sample phases.
func (r *resampler) compute(m int) float64 {
	p := 1.5 * float64(m)
	k := int(math.Floor(p))
	frac := p - float64(k)
	y0 := r.filterAt(k)
	if frac == 0 {
		return y0
	}
	y1 := r.filterAt(k + 1)
	return (y0 + y1) / 2
}

// filterAt convolves the 15 taps centered at integer position k.
func (r *resampler) filterAt(k int) float64 {
	var sum float64
	for n := 0; n < taps; n++ {
		sum += r.coeff[n] * r.sampleAt(k-tapCenter+n)
	}
	return sum
}

// sampleAt returns the input sample at absolute index i, zero-padding before 0 and after
// the final real sample count (so the tail never loses output positions).
func (r *resampler) sampleAt(i int) float64 {
	if i < 0 || i >= r.total {
		return 0
	}
	return r.hist[i-r.base]
}

// advance discards input samples no longer reachable by any future output, bounding state.
func (r *resampler) advance() {
	needStart := int(math.Floor(1.5*float64(r.next))) - tapCenter
	if needStart <= r.base {
		return
	}
	drop := needStart - r.base
	if drop > len(r.hist) {
		drop = len(r.hist)
	}
	r.hist = r.hist[drop:]
	r.base += drop
}

// floatToInt16 converts a normalized sample to signed int16 little-endian range with exact
// clamping at the extrema.
func floatToInt16(x float64) int16 {
	switch {
	case x <= -1:
		return -32768
	case x >= 1:
		return 32767
	default:
		return int16(math.Round(x * 32767))
	}
}

// int16LE encodes int16 samples to little-endian bytes.
func int16LE(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
	}
	return out
}

// framer accumulates canonical bytes and yields whole 640-byte frames.
type framer struct {
	buf []byte
}

func (f *framer) push(b []byte) [][]byte {
	var frames [][]byte
	f.buf = append(f.buf, b...)
	for len(f.buf) >= frameBytes {
		frames = append(frames, f.buf[:frameBytes])
		f.buf = f.buf[frameBytes:]
	}
	return frames
}

// flush zero-pads a residual frame to 640 bytes; returns nil when there is no residual.
func (f *framer) flush() []byte {
	if len(f.buf) == 0 {
		return nil
	}
	frame := make([]byte, frameBytes)
	copy(frame, f.buf)
	f.buf = f.buf[:0]
	return frame
}

const frameBytes = 640
