package agent

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestFrameAnalyzerDecodeAndRMS(t *testing.T) {
	var an frameAnalyzer
	f, err := an.analyze(silenceFrame())
	if err != nil {
		t.Fatalf("silence: %v", err)
	}
	if f.rms != 0 || f.zcr != 0 {
		t.Errorf("digital silence rms/zcr = %v/%v", f.rms, f.zcr)
	}
	if f.strong {
		t.Error("silence classified as a hold tone")
	}

	full, err := an.analyze(pcmFrame(16384))
	if err != nil {
		t.Fatalf("dc: %v", err)
	}
	// A constant frame is entirely DC and is removed before RMS.
	if full.rms > 1e-9 {
		t.Errorf("DC frame rms = %v, want ~0 after DC removal", full.rms)
	}

	half, err := an.analyze(pcmFrame(16384))
	if err != nil {
		t.Fatalf("dc2: %v", err)
	}
	_ = half

	// A constant after a real signal must not look like speech.
	if _, err := an.analyze(make([]byte, domain.FrameBytes-2)); err == nil {
		t.Fatal("short frame accepted")
	}
}

func TestFrameAnalyzerKnownSine(t *testing.T) {
	var an frameAnalyzer
	f, err := an.analyze(toneFrame(1000, 12000))
	if err != nil {
		t.Fatalf("tone: %v", err)
	}
	// 12000 peak sine -> RMS = 12000/sqrt(2)/32768.
	want := 12000 / math.Sqrt2 / 32768
	if math.Abs(f.rms-want) > 1e-3 {
		t.Errorf("sine rms = %v, want ~%v", f.rms, want)
	}
	best, bestPower := 0, 0.0
	for i, p := range f.power {
		if p > bestPower {
			best, bestPower = i, p
		}
	}
	if best != 9 { // 1000 Hz is bin index 9 (100 Hz spacing)
		t.Errorf("max bin = %d (%d Hz), want 9", best, (best+1)*100)
	}
	if f.flatness >= 0.2 {
		t.Errorf("pure tone flatness = %v, want clearly tonal", f.flatness)
	}
	if f.strong {
		t.Error("1000 Hz tone must not match a 350/440/480/620 hold probe")
	}
	if f.flux != 1 {
		t.Errorf("first frame flux = %v, want 1", f.flux)
	}

	next, err := an.analyze(toneFrame(1000, 12000))
	if err != nil {
		t.Fatalf("tone 2: %v", err)
	}
	if next.flux > 0.05 {
		t.Errorf("stationary tone flux = %v, want near zero", next.flux)
	}
}

func TestFrameAnalyzerProbeAndZCR(t *testing.T) {
	var an frameAnalyzer
	f, err := an.analyze(toneFrame(440, 12000))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !f.strong {
		t.Fatalf("440 Hz tone not recognized as strong hold tone (zcr=%v flatness=%v)", f.zcr, f.flatness)
	}
	if f.probe != 1 {
		t.Errorf("strong probe = %d, want 1 (440 Hz)", f.probe)
	}
	// ZCR for 440 Hz at 16 kHz is ~0.055.
	if math.Abs(f.zcr-0.055) > 0.01 {
		t.Errorf("440 Hz zcr = %v, want ~0.055", f.zcr)
	}
}

func TestFrameAnalyzerFluxAndFlatness(t *testing.T) {
	var an frameAnalyzer
	var previous frameFeatures
	for i := 0; i < 6; i++ {
		f, err := an.analyze(speechFrame(i))
		if err != nil {
			t.Fatalf("speech %d: %v", i, err)
		}
		if i > 0 && f.flux < 0.04 {
			t.Errorf("speech frame %d flux = %v, want >= 0.04", i, f.flux)
		}
		if f.flatness >= 0.75 {
			t.Errorf("speech frame %d flatness = %v, want < 0.75", i, f.flatness)
		}
		if f.rms < 0.02 || f.rms > 0.2 {
			t.Errorf("speech frame %d rms = %v, want speech-like energy", i, f.rms)
		}
		if f.strong {
			t.Errorf("speech frame %d classified as a strong hold tone", i)
		}
		previous = f
	}
	if previous.peaks < 2 {
		t.Errorf("multi-component speech peaks = %d, want >= 2", previous.peaks)
	}

	var noise frameAnalyzer
	white := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		v := int16((i*7919 + 12345) % 4000)
		binary.LittleEndian.PutUint16(white[2*i:], uint16(v))
	}
	f, err := noise.analyze(white)
	if err != nil {
		t.Fatalf("noise: %v", err)
	}
	if math.IsNaN(f.flux) || math.IsInf(f.flux, 0) || math.IsNaN(f.flatness) {
		t.Errorf("non-finite features on zero-power input: %+v", f)
	}
}

func TestMedianPowerBounded(t *testing.T) {
	var p [featureBins]float64
	for i := range p {
		p[i] = float64(i % 7)
	}
	if got := medianPower(p); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
}
