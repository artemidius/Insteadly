package agent

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// The DSP here is deliberately stdlib-only and allocation-free on the per-frame path:
// canonical PCM16 decode, DC removal, RMS, zero-crossing rate, Hann-windowed Goertzel
// powers on a fixed 39-bin grid plus four hold-tone probes, normalized positive spectral
// flux, spectral flatness, and a conservative strong-tone test. No audio library, CGO,
// FFT dependency, recording buffer, or global mutable state is involved.
const (
	featureBins    = 39 // 100 Hz spacing from 100 through 3900 Hz
	featureEpsilon = 1e-12
	// featureStrongToneRatio is the probe-to-median spectral power ratio (~12 dB) a frame
	// must exceed to count as a strong hold tone. Such a frame is never speech onset.
	featureStrongToneRatio = 16.0
)

var (
	errShortFrame = errors.New("agent: audio frame must be a complete canonical 20 ms frame")
)

// frameFeatures is one analyzed 20 ms canonical frame. It is a value type so the VAD and
// hold detector can share it without retaining audio.
type frameFeatures struct {
	rms      float64 // normalized (sample/32768) RMS after DC removal
	zcr      float64 // zero crossings per sample after DC removal
	power    [featureBins]float64
	probes   [4]float64
	flux     float64
	flatness float64
	peaks    int // stable spectral peaks in the 39-bin vector
	strong   bool
	probe    int // index of the strongest hold-tone probe, -1 when none is strong
}

// probeFreqs are the four hold-tone probes called out by the C4 contract.
var probeFreqs = [4]float64{350, 440, 480, 620}

// Hann window and Goertzel coefficients are immutable package tables, not mutable state.
var (
	hannWindow [domain.SampleRate / 50]float64 // 320 samples = 20 ms
	binCoefs   [featureBins]float64
	probeCoefs [4]float64
)

func init() {
	n := len(hannWindow)
	for i := range hannWindow {
		hannWindow[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	for k := 0; k < featureBins; k++ {
		f := 100 * float64(k+1)
		binCoefs[k] = 2 * math.Cos(2*math.Pi*f/domain.SampleRate)
	}
	for p, f := range probeFreqs {
		probeCoefs[p] = 2 * math.Cos(2*math.Pi*f/domain.SampleRate)
	}
}

// frameAnalyzer owns only the previous 39-bin magnitude vector for spectral flux. One
// analyzer belongs to one call's audio pump; it is not safe for concurrent use.
type frameAnalyzer struct {
	prev     [featureBins]float64
	havePrev bool
}

// analyze decodes and measures one complete canonical frame. Arbitrary chunk boundaries
// must be reframed by the caller before this point.
func (a *frameAnalyzer) analyze(frame []byte) (frameFeatures, error) {
	var f frameFeatures
	if len(frame) != domain.FrameBytes {
		return f, errShortFrame
	}
	n := domain.FrameBytes / 2

	var samples [domain.SampleRate / 50]float64
	var mean float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[2*i:]))) / 32768
		samples[i] = s
		mean += s
	}
	mean /= float64(n)

	var sumSquares float64
	crossings := 0
	prevSign := false
	for i := 0; i < n; i++ {
		v := samples[i] - mean
		samples[i] = v
		sumSquares += v * v
		if i > 0 {
			if (v >= 0) != prevSign {
				crossings++
			}
		}
		prevSign = v >= 0
	}
	f.rms = math.Sqrt(sumSquares / float64(n))
	if n > 1 {
		f.zcr = float64(crossings) / float64(n-1)
	}

	// Window once into a fixed scratch array; Goertzel reuses it for every bin/probe.
	var windowed [domain.SampleRate / 50]float64
	for i := 0; i < n; i++ {
		windowed[i] = samples[i] * hannWindow[i]
	}
	for k := 0; k < featureBins; k++ {
		f.power[k] = goertzelPower(windowed[:n], binCoefs[k])
	}
	for p := range probeCoefs {
		f.probes[p] = goertzelPower(windowed[:n], probeCoefs[p])
	}

	// Flux is computed on magnitude vectors; the first frame of a stream has no history
	// and is treated as fully novel so an immediate utterance is not delayed.
	if a.havePrev {
		var num, den float64
		for k := 0; k < featureBins; k++ {
			m := math.Sqrt(f.power[k])
			p := a.prev[k]
			if m > p {
				num += m - p
			}
			den += p
			a.prev[k] = m
		}
		f.flux = num / (den + featureEpsilon)
	} else {
		f.flux = 1
		for k := 0; k < featureBins; k++ {
			a.prev[k] = math.Sqrt(f.power[k])
		}
		a.havePrev = true
	}

	f.flatness = spectralFlatness(f.power)
	f.peaks = stablePeaks(f.power)
	f.probe = -1
	probeMax := -1.0
	for p, v := range f.probes {
		if v > probeMax {
			probeMax = v
			f.probe = p
		}
	}
	if probeMax > 0 {
		median := medianPower(f.power)
		maxBin := 0.0
		for _, p := range f.power {
			if p > maxBin {
				maxBin = p
			}
		}
		// The probe must dominate the median spectrum, be a meaningful share of the single
		// strongest bin (so leakage from a non-probe sine is not mistaken for a hold tone),
		// and have a compatible 350–620 Hz zero-crossing cadence.
		if probeMax >= featureStrongToneRatio*(median+featureEpsilon) &&
			probeMax >= 0.2*maxBin &&
			f.zcr >= 0.02 && f.zcr <= 0.13 {
			f.strong = true
		} else {
			f.probe = -1
		}
	} else {
		f.probe = -1
	}
	return f, nil
}

// goertzelPower computes |X(f)|^2 for one frequency using the Goertzel recurrence.
func goertzelPower(samples []float64, coeff float64) float64 {
	var s0, s1, s2 float64
	for _, x := range samples {
		s0 = x + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	return s1*s1 + s2*s2 - coeff*s1*s2
}

// spectralFlatness is the geometric/arithmetic power mean, robust to zero bins.
func spectralFlatness(power [featureBins]float64) float64 {
	var logSum, sum float64
	for _, p := range power {
		q := p + featureEpsilon
		logSum += math.Log(q)
		sum += q
	}
	geo := math.Exp(logSum / featureBins)
	arith := sum / featureBins
	if arith <= 0 {
		return 0
	}
	return geo / arith
}

// stablePeaks counts local maxima at least a tenth of the strongest bin. A pure tone has
// one; music/noise with multiple components has several.
func stablePeaks(power [featureBins]float64) int {
	max := 0.0
	for _, p := range power {
		if p > max {
			max = p
		}
	}
	if max <= 0 {
		return 0
	}
	floor := max * 0.1
	count := 0
	for k := 1; k < featureBins-1; k++ {
		if power[k] >= floor && power[k] > power[k-1] && power[k] > power[k+1] {
			count++
		}
	}
	return count
}

// medianPower returns the median of the 39 power values via insertion sort on a local
// copy; the array is small enough that no heap allocation or sort dependency is needed.
func medianPower(power [featureBins]float64) float64 {
	v := power
	for i := 1; i < featureBins; i++ {
		x := v[i]
		j := i - 1
		for j >= 0 && v[j] > x {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
	return v[featureBins/2]
}

// speechClass reports the hold detector's conservative speech-membership test for an
// isolated frame. Onset additionally requires spectral flux.
func speechClass(f frameFeatures, threshold, maxFlatness float64) bool {
	return f.rms >= threshold && f.flatness < maxFlatness && !f.strong
}
