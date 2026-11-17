package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func analyzeFrames(t *testing.T, frames [][]byte) []frameFeatures {
	t.Helper()
	var an frameAnalyzer
	out := make([]frameFeatures, len(frames))
	for i, f := range frames {
		feat, err := an.analyze(f)
		if err != nil {
			t.Fatalf("analyze %d: %v", i, err)
		}
		out[i] = feat
	}
	return out
}

func TestHoldDetectorSilenceIsNeverHold(t *testing.T) {
	h := newHoldDetector(testHoldConfig())
	feats := analyzeFrames(t, frames(200, silenceFrame()))
	for i, f := range feats {
		_, confirmed := h.observe(f, speechClass(f, 0.012, 0.75), f.rms >= 0.012)
		if confirmed {
			t.Fatalf("silence confirmed as hold at frame %d", i)
		}
	}
}

func TestHoldDetectorContinuousProbeTones(t *testing.T) {
	for _, freq := range []float64{350, 440, 480, 620} {
		h := newHoldDetector(testHoldConfig())
		feats := analyzeFrames(t, frames(150, toneFrame(freq, 12000)))
		confirmedAt := -1
		for i, f := range feats {
			_, confirmed := h.observe(f, false, true)
			if confirmed && confirmedAt < 0 {
				confirmedAt = i
			}
		}
		if confirmedAt != 149 {
			t.Errorf("%.0f Hz hold confirmed at %d, want the full 150-frame window", freq, confirmedAt)
		}
	}
}

func TestHoldDetectorCadence(t *testing.T) {
	h := newHoldDetector(testHoldConfig())
	var list [][]byte
	for i := 0; i < 8; i++ {
		list = append(list, frames(10, toneFrame(440, 12000))...)
		list = append(list, frames(10, silenceFrame())...)
	}
	list = list[:150]
	feats := analyzeFrames(t, list)
	confirmed := false
	for _, f := range feats {
		_, c := h.observe(f, false, true)
		confirmed = confirmed || c
	}
	if !confirmed {
		t.Fatal("repeated tone cadence was not detected as hold")
	}
}

func TestHoldDetectorMusicLikeAndRejections(t *testing.T) {
	// Music-like features: energy, multiple stable peaks, low spectral variation, and not
	// speech-classified. Real tone/speech fixtures cover the other paths.
	h := newHoldDetector(testHoldConfig())
	confirmed := false
	for i := 0; i < 160; i++ {
		f := frameFeatures{rms: 0.05, flux: 0.1, peaks: 2}
		_, c := h.observe(f, false, true)
		confirmed = confirmed || c
	}
	if !confirmed {
		t.Fatal("music-like window not detected")
	}

	// Speech is not hold.
	h = newHoldDetector(testHoldConfig())
	feats := analyzeFrames(t, speechFrames(160))
	for _, f := range feats {
		if _, c := h.observe(f, speechClass(f, 0.012, 0.75), f.rms >= 0.012); c {
			t.Fatal("speech classified as hold")
		}
	}

	// Steady broadband noise (high flux, no stable peaks) is not hold.
	h = newHoldDetector(testHoldConfig())
	noise := make([]byte, 640)
	for i := range noise {
		noise[i] = byte((i*73 + 19) % 256)
	}
	feats = analyzeFrames(t, frames(160, noise))
	for _, f := range feats {
		if _, c := h.observe(f, speechClass(f, 0.012, 0.75), f.rms >= 0.012); c {
			t.Fatal("broadband noise classified as hold")
		}
	}
}

func holdToneFrames(n int) [][]byte { return frames(n, toneFrame(440, 12000)) }

func TestHoldEntersWithoutSTTAndResumesOnSpeech(t *testing.T) {
	dlg := textOnlyDialogue("Gerne.")
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"Ich bin wieder da."}}}}
	cfg.Dialogue = dlg
	c := startConversation(t, cfg)
	c.call.push(holdToneFrames(defaultHoldWindowFrames)...)
	waitFor(t, "hold entered", func() bool {
		// The listener publishes no hold event to the test; observe the effect instead:
		// after hold entry a clear is not required (nothing was playing), so probe the agent
		// state through the absence of STT for a short window.
		time.Sleep(50 * time.Millisecond)
		c.agent.cfg.STT.(*fakeSTT).mu.Lock()
		defer c.agent.cfg.STT.(*fakeSTT).mu.Unlock()
		return c.agent.cfg.STT.(*fakeSTT).opens == 0
	})
	c.call.push(utteranceAudio()...)
	waitFor(t, "resume to dialogue", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	dlg.mu.Lock()
	text := dlg.texts[0]
	dlg.mu.Unlock()
	if text != "Ich bin wieder da." {
		t.Errorf("hold audio leaked into dialogue: %q", text)
	}
	close(c.ended)
	_, _ = resultOf(t, c.done)
}

func TestHoldTimeoutReturnsFailedOther(t *testing.T) {
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = &fakeSTT{}
	cfg.Dialogue = textOnlyDialogue("x")
	cfg.Hold.MaxContinuous = 40 * time.Millisecond
	c := startConversation(t, cfg)
	c.call.push(holdToneFrames(defaultHoldWindowFrames)...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("hold timeout returned an operational error: %v", err)
	}
	if outcome.Kind != domain.OutcomeFailed || outcome.FailReason != "other" || outcome.Detail != "hold timeout" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestHoldSilenceExitReturnsToListening(t *testing.T) {
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"Hallo."}}}}
	dlg := textOnlyDialogue("Gerne.")
	cfg.Dialogue = dlg
	c := startConversation(t, cfg)
	c.call.push(holdToneFrames(defaultHoldWindowFrames)...)
	c.call.push(frames(defaultHoldExitSilenceFrames+2, silenceFrame())...)
	c.call.push(utteranceAudio()...)
	waitFor(t, "dialogue after silence exit", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	close(c.ended)
	_, _ = resultOf(t, c.done)
}

func TestHoldDuringAssistantCancelsPlaybackAndClears(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Eine lange Antwort."}, waitCtx: never, waitErr: errors.New("canceled")},
		{chunks: []string{"Nach dem Hold."}},
	}}
	tts := &fakeTTS{}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"frage"}}, {finals: []string{"nach dem hold"}}}}
	cfg.Dialogue, cfg.TTS = dlg, tts
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "assistant speaks", func() bool { return ttsSpoke(tts, "lange Antwort") })
	c.call.push(holdToneFrames(defaultHoldWindowFrames)...)
	waitFor(t, "hold interrupted the turn", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.interrupts) == 1
	})
	if c.call.clearCount() != 1 {
		t.Errorf("clear count = %d, want one hold clear", c.call.clearCount())
	}
	c.call.push(utteranceAudio()...)
	waitFor(t, "resume turn", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 2
	})
	close(c.ended)
	_, _ = resultOf(t, c.done)
}

func TestHoldDetectorCadenceRequiresSimilarSameProbeBursts(t *testing.T) {
	observe := func(list [][]byte) (confirmed bool) {
		t.Helper()
		h := newHoldDetector(testHoldConfig())
		for _, f := range analyzeFrames(t, list) {
			_, c := h.observe(f, false, true)
			confirmed = confirmed || c
		}
		return confirmed
	}

	// Equal same-probe bursts are a real cadence.
	equal := flatten(
		frames(20, toneFrame(440, 12000)),
		frames(5, silenceFrame()),
		frames(20, toneFrame(440, 12000)),
		frames(105, silenceFrame()),
	)
	if !observe(equal) {
		t.Fatal("similar same-probe cadence was not detected")
	}

	// Wildly unequal bursts with the same total tone count are not a cadence.
	unequal := flatten(
		frames(5, toneFrame(440, 12000)),
		frames(2, silenceFrame()),
		frames(35, toneFrame(440, 12000)),
		frames(108, silenceFrame()),
	)
	if observe(unequal) {
		t.Fatal("unequal single-dominant burst was misclassified as cadence")
	}

	// Alternating different probes are not a same-probe cadence.
	mixed := flatten(
		frames(20, toneFrame(350, 12000)),
		frames(5, silenceFrame()),
		frames(20, toneFrame(440, 12000)),
		frames(105, silenceFrame()),
	)
	if observe(mixed) {
		t.Fatal("mixed-probe tone sequence was misclassified as cadence")
	}
}
