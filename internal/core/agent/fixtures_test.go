package agent

import (
	"encoding/binary"
	"math"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Canonical PCM fixture helpers. All audio is generated locally; no recording, patient, or
// human data is checked in.

func pcmFrame(amplitude int16) []byte {
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(amplitude))
	}
	return frame
}

func silenceFrame() []byte { return make([]byte, domain.FrameBytes) }

func frames(n int, frame []byte) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = append([]byte(nil), frame...)
	}
	return out
}

func flatten(chunks ...[][]byte) [][]byte {
	var out [][]byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// toneFrame is a stationary sine at freq with the given peak amplitude (PCM units).
func toneFrame(freq float64, amplitude int16) []byte {
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		v := int16(float64(amplitude) * math.Sin(2*math.Pi*freq*float64(i)/domain.SampleRate))
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(v))
	}
	return frame
}

// speechFrame generates a non-stationary multi-component voiced/noisy frame: several
// non-probe sine components whose amplitudes vary with the seed. It has speech-like energy,
// sub-unity spectral flatness, and spectral flux between frames, so VAD fixtures do not
// depend on a real speaker.
func speechFrame(seed int) []byte {
	freqs := [6]float64{220, 730, 1150, 1750, 2650, 3300}
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		var v float64
		for k := 0; k < len(freqs); k++ {
			amp := 0.10 + 0.08*math.Sin(float64(seed*7+k*3))
			phase := float64(seed*11+k*5) * 0.37
			v += amp * math.Sin(2*math.Pi*freqs[k]*float64(i)/domain.SampleRate+phase)
		}
		sample := int16(v * 18000)
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(sample))
	}
	return frame
}

func speechFrames(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = speechFrame(i)
	}
	return out
}

// utteranceAudio is the deterministic conversation fixture: 20 voiced frames (well above
// the 10-frame minimum) followed by 35 silence frames (the 700 ms hangover plus one).
func utteranceAudio() [][]byte {
	return flatten(speechFrames(20), frames(35, silenceFrame()))
}

func testVADConfig() VADConfig {
	var c VADConfig
	if err := applyVADDefaults(&c); err != nil {
		panic(err)
	}
	return c
}

func testHoldConfig() HoldConfig {
	var c HoldConfig
	if err := applyHoldDefaults(&c); err != nil {
		panic(err)
	}
	return c
}
