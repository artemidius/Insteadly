package agent

import (
	"errors"
	"fmt"
	"math"
)

// VAD settings are reviewable core constants. They are never serialized into health or
// config summaries and are only overridden through agent.Config in tests.
const (
	defaultPreRollFrames       = 15
	defaultStartFrames         = 3
	defaultBargeInFrames       = 5
	defaultMinActiveFrames     = 10
	defaultEndSilenceFrames    = 35
	defaultMaxUtteranceFrames  = 750
	defaultMinSpeechRMS        = 0.012
	defaultBargeInMinRMS       = 0.024
	defaultNoiseRatio          = 3.0
	defaultBargeInNoiseRatio   = 4.0
	defaultMinSpectralFlux     = 0.04
	defaultMaxSpectralFlatness = 0.75

	initialNoiseRMS = 0.003
	minNoiseRMS     = 0.001
	maxNoiseRMS     = 0.05
	noiseRiseAlpha  = 0.02
	noiseFallAlpha  = 0.005
	maxSpeechRMS    = 0.10
)

// VADConfig groups the streaming VAD thresholds. Zero values use the defaults above;
// negative or impossible combinations are rejected by agent.New.
type VADConfig struct {
	PreRollFrames       int
	StartFrames         int
	BargeInFrames       int
	MinActiveFrames     int
	EndSilenceFrames    int
	MaxUtteranceFrames  int
	MinSpeechRMS        float64
	BargeInMinRMS       float64
	NoiseRatio          float64
	BargeInNoiseRatio   float64
	MinSpectralFlux     float64
	MaxSpectralFlatness float64
}

func applyVADDefaults(c *VADConfig) error {
	if err := nonNegative("vad pre-roll frames", c.PreRollFrames); err != nil {
		return err
	}
	if c.PreRollFrames == 0 {
		c.PreRollFrames = defaultPreRollFrames
	}
	if err := nonNegative("vad start frames", c.StartFrames); err != nil {
		return err
	}
	if c.StartFrames == 0 {
		c.StartFrames = defaultStartFrames
	}
	if err := nonNegative("vad barge-in frames", c.BargeInFrames); err != nil {
		return err
	}
	if c.BargeInFrames == 0 {
		c.BargeInFrames = defaultBargeInFrames
	}
	if err := nonNegative("vad minimum active frames", c.MinActiveFrames); err != nil {
		return err
	}
	if c.MinActiveFrames == 0 {
		c.MinActiveFrames = defaultMinActiveFrames
	}
	if err := nonNegative("vad end-silence frames", c.EndSilenceFrames); err != nil {
		return err
	}
	if c.EndSilenceFrames == 0 {
		c.EndSilenceFrames = defaultEndSilenceFrames
	}
	if err := nonNegative("vad maximum utterance frames", c.MaxUtteranceFrames); err != nil {
		return err
	}
	if c.MaxUtteranceFrames == 0 {
		c.MaxUtteranceFrames = defaultMaxUtteranceFrames
	}
	if err := nonNegativeFloat("vad minimum speech rms", c.MinSpeechRMS); err != nil {
		return err
	}
	if c.MinSpeechRMS == 0 {
		c.MinSpeechRMS = defaultMinSpeechRMS
	}
	if err := nonNegativeFloat("vad barge-in minimum rms", c.BargeInMinRMS); err != nil {
		return err
	}
	if c.BargeInMinRMS == 0 {
		c.BargeInMinRMS = defaultBargeInMinRMS
	}
	if err := nonNegativeFloat("vad noise ratio", c.NoiseRatio); err != nil {
		return err
	}
	if c.NoiseRatio == 0 {
		c.NoiseRatio = defaultNoiseRatio
	}
	if err := nonNegativeFloat("vad barge-in noise ratio", c.BargeInNoiseRatio); err != nil {
		return err
	}
	if c.BargeInNoiseRatio == 0 {
		c.BargeInNoiseRatio = defaultBargeInNoiseRatio
	}
	if err := nonNegativeFloat("vad minimum spectral flux", c.MinSpectralFlux); err != nil {
		return err
	}
	if c.MinSpectralFlux == 0 {
		c.MinSpectralFlux = defaultMinSpectralFlux
	}
	if err := nonNegativeFloat("vad maximum spectral flatness", c.MaxSpectralFlatness); err != nil {
		return err
	}
	if c.MaxSpectralFlatness == 0 {
		c.MaxSpectralFlatness = defaultMaxSpectralFlatness
	}

	if c.StartFrames > c.PreRollFrames {
		return errors.New("agent: vad start frames must not exceed pre-roll frames")
	}
	if c.BargeInFrames > c.PreRollFrames {
		return errors.New("agent: vad barge-in frames must not exceed pre-roll frames")
	}
	if c.StartFrames > c.MinActiveFrames {
		return errors.New("agent: vad start frames must not exceed minimum active frames")
	}
	if c.MinActiveFrames > c.MaxUtteranceFrames {
		return errors.New("agent: vad minimum active frames must not exceed maximum utterance frames")
	}
	if c.EndSilenceFrames > c.MaxUtteranceFrames {
		return errors.New("agent: vad end-silence frames must not exceed maximum utterance frames")
	}
	if c.PreRollFrames > c.MaxUtteranceFrames {
		return errors.New("agent: vad pre-roll frames must not exceed maximum utterance frames")
	}
	if c.NoiseRatio <= 0 || c.BargeInNoiseRatio <= 0 {
		return errors.New("agent: vad noise ratios must be positive")
	}
	if c.BargeInNoiseRatio < c.NoiseRatio {
		return errors.New("agent: vad barge-in noise ratio must not be weaker than the normal ratio")
	}
	if c.BargeInMinRMS < c.MinSpeechRMS {
		return errors.New("agent: vad barge-in threshold must not be weaker than the normal threshold")
	}
	if c.MinSpectralFlux <= 0 || c.MinSpectralFlux >= 1 {
		return errors.New("agent: vad minimum spectral flux must be in (0,1)")
	}
	if c.MaxSpectralFlatness <= 0 || c.MaxSpectralFlatness > 1 {
		return errors.New("agent: vad maximum spectral flatness must be in (0,1]")
	}
	if c.MinSpeechRMS >= maxSpeechRMS {
		return errors.New("agent: vad minimum speech rms must stay below the normal energy cap")
	}
	return nil
}

func nonNegativeFloat(name string, v float64) error {
	if v < 0 {
		return fmt.Errorf("agent: %s must not be negative", name)
	}
	return nil
}

// vadMode selects normal turn-taking or stronger barge-in onset thresholds.
type vadMode int

const (
	vadListen vadMode = iota
	vadBarge
)

// vadResult is one frame's streaming decision. Forward holds the frames the audio pump
// must feed to the STT worker for this frame: the bounded pre-roll burst exactly once on
// onset, otherwise the current frame.
type vadResult struct {
	Features frameFeatures
	Speech   bool
	Active   bool
	Onset    bool
	BargeIn  bool
	End      bool
	Discard  bool
	Forward  [][]byte
}

// vad owns adaptive noise, pre-roll, onset/hangover, and the forced utterance bound. It
// is independent of channels and providers; one instance belongs to one call.
type vad struct {
	cfg VADConfig
	an  frameAnalyzer

	noise float64

	pre       [][]byte
	preActive []bool
	tail      [][]byte
	onsetRun  int

	inUtterance  bool
	activeFrames int
	silenceRun   int
	fedFrames    int
}

func newVAD(cfg VADConfig) *vad {
	return &vad{cfg: cfg, noise: initialNoiseRMS}
}

// noiseRMS exposes the adapted floor for tests and the hold detector.
func (v *vad) noiseRMS() float64 { return v.noise }

// energyThreshold is the normal speech energy threshold.
func (v *vad) energyThreshold() float64 {
	return clamp(v.cfg.NoiseRatio*v.noise, v.cfg.MinSpeechRMS, maxSpeechRMS)
}

// bargeThreshold is deliberately stronger than the normal threshold so assistant echo
// and background noise do not trigger interruptions.
func (v *vad) bargeThreshold() float64 {
	return math.Max(math.Max(v.energyThreshold(), v.cfg.BargeInNoiseRatio*v.noise), v.cfg.BargeInMinRMS)
}

// onsetFrame reports whether f is a speech-onset frame under the selected mode.
func (v *vad) onsetFrame(f frameFeatures, mode vadMode) bool {
	threshold := v.energyThreshold()
	if mode == vadBarge {
		threshold = v.bargeThreshold()
	}
	return f.rms >= threshold &&
		f.flatness < v.cfg.MaxSpectralFlatness &&
		f.flux >= v.cfg.MinSpectralFlux &&
		!f.strong
}

// continuationFrame reports sustained speech: energy above the normal threshold and no
// strong hold tone. Flux is not required for sustained vowels.
func (v *vad) continuationFrame(f frameFeatures) bool {
	return f.rms >= v.energyThreshold() && !f.strong
}

// push consumes exactly one complete canonical frame. freeze suppresses noise-floor
// training for assistant playback and hold candidates so the agent's own echo cannot
// train the floor.
func (v *vad) push(frame []byte, mode vadMode, freeze bool) (vadResult, error) {
	f, err := v.an.analyze(frame)
	if err != nil {
		return vadResult{}, err
	}
	r := vadResult{Features: f}
	r.Active = f.rms >= v.energyThreshold()
	r.Speech = speechClass(f, v.energyThreshold(), v.cfg.MaxSpectralFlatness)

	if !v.inUtterance {
		onset := v.onsetFrame(f, mode)
		if !onset {
			// Non-speech, non-hold frames train the adaptive floor. A strong tone is a hold
			// candidate and never trains it.
			if !freeze && !f.strong {
				v.adapt(f.rms)
			}
			v.onsetRun = 0
			v.pushPre(frame, v.continuationFrame(f))
			return r, nil
		}

		v.pushPre(frame, v.continuationFrame(f))
		v.onsetRun++
		needed := v.cfg.StartFrames
		if mode == vadBarge {
			needed = v.cfg.BargeInFrames
		}
		if v.onsetRun < needed {
			return r, nil
		}

		// Confirmed onset: burst the bounded pre-roll exactly once, including the onset
		// frames. The pump forwards these to a fresh per-utterance STT stream.
		v.inUtterance = true
		v.onsetRun = 0
		v.activeFrames = 0
		v.silenceRun = 0
		v.fedFrames = 0
		r.Forward = cloneFrames(v.pre)
		v.fedFrames = len(r.Forward)
		for _, active := range v.preActive {
			if active {
				v.activeFrames++
			}
		}
		v.tail = v.tail[:0]
		for _, pre := range v.pre {
			v.remember(pre)
		}
		v.pre = v.pre[:0]
		v.preActive = v.preActive[:0]
		r.Onset = true
		r.BargeIn = mode == vadBarge
		return r, nil
	}

	// Inside an utterance every frame is forwarded; the STT worker already has the
	// pre-roll from onset.
	r.Forward = [][]byte{frame}
	v.fedFrames++
	v.remember(frame)
	if v.continuationFrame(f) {
		v.activeFrames++
		v.silenceRun = 0
	} else {
		v.silenceRun++
	}

	switch {
	case v.fedFrames >= v.cfg.MaxUtteranceFrames:
		r.End = true
		if v.activeFrames < v.cfg.MinActiveFrames {
			r.Discard = true
		} else {
			v.seedContinuingSpeech(mode)
		}
		v.resetUtterance()
	case v.activeFrames >= v.cfg.MinActiveFrames && v.silenceRun >= v.cfg.EndSilenceFrames:
		r.End = true
		v.resetUtterance()
	case v.activeFrames < v.cfg.MinActiveFrames && v.silenceRun >= v.cfg.EndSilenceFrames:
		// A cough/noise candidate never reached the 200 ms minimum: close its worker and
		// discard the transcript, remaining in the same listen window.
		r.End = true
		r.Discard = true
		v.resetUtterance()
	}
	return r, nil
}

func (v *vad) pushPre(frame []byte, active bool) {
	v.pre = append(v.pre, append([]byte(nil), frame...))
	v.preActive = append(v.preActive, active)
	if len(v.pre) > v.cfg.PreRollFrames {
		v.pre = v.pre[1:]
		v.preActive = v.preActive[1:]
	}
}

func (v *vad) remember(frame []byte) {
	v.tail = append(v.tail, append([]byte(nil), frame...))
	if len(v.tail) > v.cfg.PreRollFrames {
		v.tail = v.tail[1:]
	}
}

func (v *vad) resetUtterance() {
	v.inUtterance = false
	v.activeFrames = 0
	v.silenceRun = 0
	v.fedFrames = 0
	v.pre = v.pre[:0]
	v.preActive = v.preActive[:0]
	v.tail = v.tail[:0]
	v.onsetRun = 0
}

// seedContinuingSpeech preserves recent speech as pre-roll for the next forced boundary
// so a 15 s split can continue from active speech without an artificial silence gap.
func (v *vad) seedContinuingSpeech(mode vadMode) {
	v.pre = v.pre[:0]
	v.preActive = v.preActive[:0]
	for _, frame := range v.tail {
		v.pre = append(v.pre, append([]byte(nil), frame...))
		v.preActive = append(v.preActive, false)
	}
	v.onsetRun = v.cfg.StartFrames - 1
	if mode == vadBarge {
		v.onsetRun = v.cfg.BargeInFrames - 1
	}
}

func (v *vad) adapt(rms float64) {
	alpha := noiseFallAlpha
	if rms > v.noise {
		alpha = noiseRiseAlpha
	}
	v.noise = clamp((1-alpha)*v.noise+alpha*rms, minNoiseRMS, maxNoiseRMS)
}

func cloneFrames(frames [][]byte) [][]byte {
	out := make([][]byte, len(frames))
	for i, f := range frames {
		out[i] = append([]byte(nil), f...)
	}
	return out
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
