package agent

import (
	"errors"
	"fmt"
	"time"
)

const (
	defaultHoldWindowFrames      = 150 // 3 s of 20 ms frames
	defaultHoldExitSilenceFrames = 25  // 500 ms
	defaultHoldMaxContinuous     = 90 * time.Second

	// Initial deterministic fixtures, not a claim to recognize every carrier tone.
	holdToneContinuousFrames = 120
	holdToneCadenceFrames    = 40
	holdMusicActivePercent   = 90
	holdMusicSpeechPercent   = 20
	holdMusicPeaksPercent    = 80
)

// HoldConfig groups the minimal M1 hold classifier settings. Zero values use defaults.
type HoldConfig struct {
	WindowFrames      int
	ExitSilenceFrames int
	MaxContinuous     time.Duration
}

func applyHoldDefaults(c *HoldConfig) error {
	if err := nonNegative("hold window frames", c.WindowFrames); err != nil {
		return err
	}
	if c.WindowFrames == 0 {
		c.WindowFrames = defaultHoldWindowFrames
	}
	if err := nonNegative("hold exit-silence frames", c.ExitSilenceFrames); err != nil {
		return err
	}
	if c.ExitSilenceFrames == 0 {
		c.ExitSilenceFrames = defaultHoldExitSilenceFrames
	}
	if err := nonNegativeDuration("hold maximum continuous", c.MaxContinuous); err != nil {
		return err
	}
	if c.MaxContinuous == 0 {
		c.MaxContinuous = defaultHoldMaxContinuous
	}
	if c.WindowFrames < 10 {
		return errors.New("agent: hold window must be at least 10 frames")
	}
	if c.WindowFrames > 600 {
		return fmt.Errorf("agent: hold window must not exceed 600 frames")
	}
	if c.ExitSilenceFrames > c.WindowFrames {
		return errors.New("agent: hold exit-silence frames must not exceed the window")
	}
	return nil
}

// holdFrame is the bounded per-frame summary retained by the 3 s hold window. No audio
// is stored.
type holdFrame struct {
	tone   bool
	probe  int
	active bool
	speech bool
	flux   float64
	peaks  int
}

// holdDetector runs a bounded slate over the last WindowFrames. It never launches a timer
// per frame; the caller reevaluates once per frame and owns the entry/exit policy.
type holdDetector struct {
	cfg     HoldConfig
	frames  []holdFrame
	next    int
	count   int
	held    bool
	lastHit bool
}

func newHoldDetector(cfg HoldConfig) *holdDetector {
	return &holdDetector{cfg: cfg, frames: make([]holdFrame, cfg.WindowFrames)}
}

// observe records one frame and reports whether the slate currently looks like hold and
// whether a full qualifying window confirms it. It is idempotent per frame and does not
// allocate.
func (h *holdDetector) observe(f frameFeatures, speech, active bool) (candidate, confirmed bool) {
	h.frames[h.next] = holdFrame{
		tone:   f.strong,
		probe:  f.probe,
		active: active,
		speech: speech,
		flux:   f.flux,
		peaks:  f.peaks,
	}
	h.next = (h.next + 1) % len(h.frames)
	if h.count < len(h.frames) {
		h.count++
	}

	full := h.count == len(h.frames)
	candidate, confirmed = h.evaluate(full)
	h.lastHit = candidate
	if confirmed {
		h.held = true
	}
	return candidate, confirmed
}

// candidate reports the last frame's candidate status so the audio pump can freeze the
// noise floor before the next frame.
func (h *holdDetector) candidate() bool { return h.lastHit }

func (h *holdDetector) evaluate(full bool) (candidate, confirmed bool) {
	if h.count == 0 {
		return false, false
	}
	start := 0
	if h.count == len(h.frames) {
		start = h.next // oldest slot in the ring
	}

	toneByProbe := [4]int{}
	tones := 0
	activeFrames := 0
	speechFrames := 0
	peakFrames := 0
	fluxFrames := 0

	type toneRun struct {
		on bool
		n  int
	}
	var runs []toneRun
	run := toneRun{}
	haveRun := false
	for i := 0; i < h.count; i++ {
		fr := h.frames[(start+i)%len(h.frames)]
		// Run-length accounting is chronological so cadence similarity is meaningful even
		// after the ring wrapped.
		if !haveRun {
			run = toneRun{on: fr.tone, n: 1}
			haveRun = true
		} else if run.on == fr.tone {
			run.n++
		} else {
			runs = append(runs, run)
			run = toneRun{on: fr.tone, n: 1}
		}

		if fr.tone {
			tones++
			if fr.probe >= 0 && fr.probe < len(toneByProbe) {
				toneByProbe[fr.probe]++
			}
		}
		if fr.active {
			activeFrames++
		}
		if fr.speech {
			speechFrames++
		}
		if fr.peaks >= 2 {
			peakFrames++
		}
		if fr.flux >= 0.005 && fr.flux <= 0.35 {
			fluxFrames++
		}
	}
	if haveRun {
		runs = append(runs, run)
	}

	maxTone := 0
	for _, n := range toneByProbe {
		if n > maxTone {
			maxTone = n
		}
	}

	toneContinuous := maxTone >= holdToneContinuousFrames
	// Cadence requires the same probe, at least two similar on-bursts separated by an
	// audible off gap. Total tone count or a single long burst alone is not a cadence.
	sameProbe := tones > 0 && maxTone*100 >= 80*tones
	onLengths := make([]int, 0, len(runs))
	internalOff := false
	for i, r := range runs {
		if r.on {
			onLengths = append(onLengths, r.n)
			continue
		}
		// An off run is an internal gap only when a tone run precedes and follows it.
		if i > 0 && i < len(runs)-1 && runs[i-1].on && runs[i+1].on && r.n >= 2 {
			internalOff = true
		}
	}
	similarBursts := false
	if len(onLengths) >= 2 {
		first, second := 0, 0
		for _, n := range onLengths {
			switch {
			case n >= first:
				second, first = first, n
			case n > second:
				second = n
			}
		}
		if second >= 2 {
			diff := first - second
			if diff < 0 {
				diff = -diff
			}
			tolerance := first / 2
			if tolerance < 3 {
				tolerance = 3
			}
			similarBursts = diff <= tolerance
		}
	}
	cadence := tones >= holdToneCadenceFrames && sameProbe && similarBursts && internalOff

	music := activeFrames*100 >= holdMusicActivePercent*h.count &&
		speechFrames*100 < holdMusicSpeechPercent*h.count &&
		peakFrames*100 >= holdMusicPeaksPercent*h.count &&
		fluxFrames >= 2
	silent := activeFrames*100 < 10*h.count

	candidate = !silent && (toneContinuous || cadence || music || maxTone >= holdToneCadenceFrames)
	if !full || silent {
		return candidate, false
	}
	return candidate, toneContinuous || cadence || music
}

// reset clears the window after a hold exit so a new hold needs a fresh full 3 s window.
func (h *holdDetector) reset() {
	h.next = 0
	h.count = 0
	h.held = false
	h.lastHit = false
}
