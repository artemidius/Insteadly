package agent

import (
	"math"
	"testing"
)

// pushFrames feeds frames and records the onset/end decisions.
type vadTrace struct {
	onsetAt   int
	endAt     int
	discard   bool
	forwarded int
	burst     int
	active    int
	onsets    int
}

func runVAD(t *testing.T, cfg VADConfig, mode vadMode, framesList [][]byte) vadTrace {
	t.Helper()
	v := newVAD(cfg)
	var tr vadTrace
	for i, frame := range framesList {
		r, err := v.push(frame, mode, false)
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		if r.Onset {
			tr.onsets++
			tr.burst = len(r.Forward)
			if tr.onsetAt == 0 {
				tr.onsetAt = i
			}
		}
		if r.End {
			if tr.endAt == 0 {
				tr.endAt = i
			}
			tr.discard = tr.discard || r.Discard
		}
		tr.forwarded += len(r.Forward)
		if r.Active {
			tr.active++
		}
	}
	return tr
}

func TestVADOnsetAfterSixtyMilliseconds(t *testing.T) {
	cfg := testVADConfig()
	tr := runVAD(t, cfg, vadListen, flatten(frames(15, silenceFrame()), speechFrames(5)))
	if tr.onsets != 1 {
		t.Fatalf("onsets = %d, want 1", tr.onsets)
	}
	// Onset frames are indices 15,16,17; onset is confirmed on the third.
	if tr.onsetAt != 17 {
		t.Errorf("onset at frame %d, want 17 (60 ms)", tr.onsetAt)
	}
	if tr.burst != cfg.PreRollFrames {
		t.Errorf("pre-roll burst = %d, want %d exactly once", tr.burst, cfg.PreRollFrames)
	}
}

func TestVADNoiseCandidateDiscarded(t *testing.T) {
	// Three onset frames then silence never reaches the 200 ms minimum: no utterance is
	// produced, and the same listen window continues.
	cfg := testVADConfig()
	list := flatten(speechFrames(3), frames(35, silenceFrame()))
	tr := runVAD(t, cfg, vadListen, list)
	if tr.onsets != 1 {
		t.Fatalf("onsets = %d", tr.onsets)
	}
	if tr.endAt == 0 || !tr.discard {
		t.Fatalf("short candidate was not discarded: %+v", tr)
	}
}

func TestVADMinimumActiveAndHangover(t *testing.T) {
	cfg := testVADConfig()
	// 15 pre-roll silence + 10 speech + 34 silence (not yet end) + 1 silence (end).
	list := flatten(frames(15, silenceFrame()), speechFrames(10), frames(35, silenceFrame()))
	tr := runVAD(t, cfg, vadListen, list)
	if tr.onsets != 1 || tr.endAt == 0 || tr.discard {
		t.Fatalf("trace = %+v", tr)
	}
	// Onset is confirmed at index 17; the pre-roll carries the 15 preceding frames and
	// the remaining 7 speech frames precede the 35-frame hangover, so the utterance closes
	// at index 59.
	wantEnd := 59
	if tr.endAt != wantEnd {
		t.Errorf("end at %d, want %d", tr.endAt, wantEnd)
	}
}

func TestVADForcedBoundaryContinuesSpeech(t *testing.T) {
	cfg := testVADConfig()
	cfg.MaxUtteranceFrames = 40
	// 3 start frames + 60 continuous speech frames: a forced boundary at 40, then the
	// continuing speech can open the next utterance without a silence gap.
	tr := runVAD(t, cfg, vadListen, speechFrames(80))
	if tr.onsets < 2 {
		t.Fatalf("onsets = %d, want a forced continuation", tr.onsets)
	}
	if tr.discard {
		t.Error("forced boundary discarded active speech")
	}
}

func TestVADNoiseFloorAdaptsAndFreezes(t *testing.T) {
	cfg := testVADConfig()
	v := newVAD(cfg)
	initial := v.noiseRMS()
	for i := 0; i < 300; i++ {
		if _, err := v.push(silenceFrame(), vadListen, false); err != nil {
			t.Fatal(err)
		}
	}
	if v.noiseRMS() >= initial {
		t.Errorf("noise floor did not fall on silence: %v -> %v", initial, v.noiseRMS())
	}
	// Rising broadband noise trains upward within the clamp.
	noisy := make([]byte, domainFrameBytes())
	for i := range noisy {
		noisy[i] = byte(i % 251)
	}
	before := v.noiseRMS()
	for i := 0; i < 300; i++ {
		if _, err := v.push(noisy, vadListen, false); err != nil {
			t.Fatal(err)
		}
	}
	if v.noiseRMS() <= before {
		t.Errorf("noise floor did not adapt upward: %v -> %v", before, v.noiseRMS())
	}

	// Freeze during assistant playback / hold.
	v2 := newVAD(cfg)
	frozen := v2.noiseRMS()
	for i := 0; i < 100; i++ {
		if _, err := v2.push(speechFrame(i), vadListen, true); err != nil {
			t.Fatal(err)
		}
	}
	if math.Abs(v2.noiseRMS()-frozen) > 1e-12 {
		t.Errorf("frozen noise floor changed: %v -> %v", frozen, v2.noiseRMS())
	}
}

func TestVADStrongToneNeverOnset(t *testing.T) {
	cfg := testVADConfig()
	for _, freq := range []float64{350, 440, 480, 620} {
		tr := runVAD(t, cfg, vadListen, frames(30, toneFrame(freq, 12000)))
		if tr.onsets != 0 {
			t.Errorf("%.0f Hz hold tone triggered an utterance", freq)
		}
	}
}

func TestVADBargeThreshold(t *testing.T) {
	cfg := testVADConfig()
	// Normal onset is 3 frames; barge-in needs 3 consecutive barge frames (60 ms).
	tr := runVAD(t, cfg, vadBarge, speechFrames(2))
	if tr.onsets != 0 {
		t.Errorf("barge fired before 60 ms: %+v", tr)
	}
	tr = runVAD(t, cfg, vadBarge, speechFrames(4))
	if tr.onsets != 1 {
		t.Fatalf("barge did not fire after 3 barge frames: %+v", tr)
	}
}

func TestVADConfigValidation(t *testing.T) {
	var c VADConfig
	if err := applyVADDefaults(&c); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if c.PreRollFrames != 15 || c.StartFrames != 3 || c.BargeInFrames != 3 ||
		c.MinActiveFrames != 10 || c.EndSilenceFrames != 35 || c.MaxUtteranceFrames != 750 {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.MinSpeechRMS != 0.012 || c.BargeInMinRMS != 0.024 || c.NoiseRatio != 3 ||
		c.BargeInNoiseRatio != 4 || c.MinSpectralFlux != 0.04 || c.MaxSpectralFlatness != 0.75 {
		t.Errorf("unexpected float defaults: %+v", c)
	}
	for _, mutate := range []func(*VADConfig){
		func(x *VADConfig) { x.PreRollFrames = -1 },
		func(x *VADConfig) { x.StartFrames = 100 },
		func(x *VADConfig) { x.BargeInFrames = 100 },
		func(x *VADConfig) { x.MinActiveFrames = 1000 },
		func(x *VADConfig) { x.EndSilenceFrames = 1000 },
		func(x *VADConfig) { x.NoiseRatio = -1 },
		func(x *VADConfig) { x.BargeInNoiseRatio = 1 },
		func(x *VADConfig) { x.BargeInMinRMS = 0.001 },
		func(x *VADConfig) { x.MinSpectralFlux = 1.5 },
		func(x *VADConfig) { x.MaxSpectralFlatness = 2 },
	} {
		cfg := c
		mutate(&cfg)
		if err := applyVADDefaults(&cfg); err == nil {
			t.Errorf("invalid VAD config accepted: %+v", cfg)
		}
	}
}

func domainFrameBytes() int { return 640 }

func TestVADBargeFiresOnSteadyLoudVoicedFramesWithoutFlux(t *testing.T) {
	// Repeating one stationary voiced frame keeps spectral flux near zero after the first
	// frame. Real speech runs like this between transitions; requiring flux on every barge
	// frame made barge essentially unreachable. The stronger energy/tone checks still apply.
	cfg := testVADConfig()
	tr := runVAD(t, cfg, vadBarge, frames(3, speechFrame(0)))
	if tr.onsets != 1 {
		t.Fatalf("barge did not fire on steady loud voiced frames: %+v", tr)
	}
	if tr.onsetAt != 2 {
		t.Errorf("barge onset at %d, want the third frame (60 ms)", tr.onsetAt)
	}
}

func TestVADBargeFiresOnMeasuredSpeechProfile(t *testing.T) {
	// RMS profile measured from real Mistral TTS "Guten Tag. …": four loud frames, a
	// consonant/phoneme dip, then four loud frames. Only three consecutive barge frames are
	// required, so the first run must open the interruption.
	profile := []float64{0.0046, 0.0373, 0.0946, 0.0897, 0.0366, 0.0035, 0.0014, 0.0054, 0.1160, 0.1388, 0.1509, 0.1167}
	list := make([][]byte, 0, len(profile))
	for _, rms := range profile {
		list = append(list, voicedFrameAtRMS(rms))
	}
	tr := runVAD(t, testVADConfig(), vadBarge, list)
	if tr.onsets != 1 {
		t.Fatalf("barge did not fire on the measured speech profile: %+v", tr)
	}
	if tr.onsetAt != 3 {
		t.Errorf("barge onset at %d, want the fourth profile frame after three confirmed frames", tr.onsetAt)
	}
}
