package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// gateConfig holds the M0 gate parameters. Defaults come from agent.Config.
type gateConfig struct {
	speechThreshold    float64
	preRollFrames      int
	startFrames        int
	minActiveFrames    int
	endSilenceFrames   int
	maxUtteranceFrames int
	listenTimeout      time.Duration
}

// gate delimits one utterance from canonical PCM16LE frames. It is intentionally a
// simple energy/silence M0 delimiter, not production VAD, and owns a bounded residual
// buffer plus a bounded pre-roll ring. State carries across utterances so leftover
// complete frames from a chunk are not lost.
type gate struct {
	cfg      gateConfig
	residual []byte
	pending  [][]byte
	pre      [][]byte
}

func newGate(cfg gateConfig) *gate {
	return &gate{cfg: cfg}
}

// utterance is one delimited set of canonical frames plus its active-frame count.
type utterance struct {
	frames [][]byte
	active int
}

// next waits for one utterance. It returns ErrCallEnded when the call ends first, an
// error when the caller context/listen window expires or the provider violates the audio
// contract, and otherwise a bounded utterance of exact 640-byte frames.
func (g *gate) next(ctx context.Context, inbound <-chan domain.AudioChunk, ended <-chan domain.CallEvent) (utterance, error) {
	listenTimer := time.NewTimer(g.cfg.listenTimeout)
	defer listenTimer.Stop()

	g.pre = g.pre[:0]
	var frames [][]byte
	activeTotal := 0
	consecutiveActive := 0
	consecutiveInactive := 0
	started := false

	for {
		select {
		case <-ended:
			return utterance{}, ErrCallEnded
		default:
		}
		select {
		case <-listenTimer.C:
			return utterance{}, errListenTimeout
		default:
		}

		frame, ok, err := g.nextFrame()
		if err != nil {
			return utterance{}, err
		}
		if !ok {
			select {
			case chunk, open := <-inbound:
				if !open {
					// Prefer a terminal call event if it raced with channel closure.
					select {
					case <-ended:
						return utterance{}, ErrCallEnded
					default:
					}
					return utterance{}, errInboundClosed
				}
				if len(chunk.Data) == 0 || len(chunk.Data)%2 != 0 {
					return utterance{}, fmt.Errorf("agent: inbound chunk must be non-empty even-length PCM16 (got %d bytes)", len(chunk.Data))
				}
				if err := g.appendChunk(chunk.Data); err != nil {
					return utterance{}, err
				}
				continue
			case <-ended:
				return utterance{}, ErrCallEnded
			case <-ctx.Done():
				return utterance{}, ctx.Err()
			case <-listenTimer.C:
				return utterance{}, errListenTimeout
			}
		}

		active := frameActive(frame, g.cfg.speechThreshold)
		if !started {
			g.pre = append(g.pre, frame)
			if len(g.pre) > g.cfg.preRollFrames {
				g.pre = g.pre[1:]
			}
			if active {
				consecutiveActive++
			} else {
				consecutiveActive = 0
			}
			if consecutiveActive < g.cfg.startFrames {
				continue
			}
			// Speech started: prepend the bounded pre-roll exactly once.
			started = true
			frames = append(frames, g.pre...)
			for _, f := range g.pre {
				if frameActive(f, g.cfg.speechThreshold) {
					activeTotal++
				}
			}
			g.pre = g.pre[:0]
			consecutiveActive = 0
			consecutiveInactive = 0
			if len(frames) >= g.cfg.maxUtteranceFrames {
				if activeTotal >= g.cfg.minActiveFrames {
					return g.finish(frames, activeTotal), nil
				}
				frames, activeTotal = nil, 0
				started = false
			}
			continue
		}

		frames = append(frames, frame)
		if active {
			activeTotal++
			consecutiveInactive = 0
		} else {
			consecutiveInactive++
		}

		if len(frames) >= g.cfg.maxUtteranceFrames {
			if activeTotal >= g.cfg.minActiveFrames {
				return g.finish(frames, activeTotal), nil
			}
			frames, activeTotal = nil, 0
			consecutiveActive, consecutiveInactive = 0, 0
			started = false
			continue
		}
		if activeTotal >= g.cfg.minActiveFrames && consecutiveInactive >= g.cfg.endSilenceFrames {
			return g.finish(frames, activeTotal), nil
		}
		if activeTotal < g.cfg.minActiveFrames && consecutiveInactive >= g.cfg.endSilenceFrames {
			// Too short to be an utterance: discard it and keep listening inside the
			// same listen window rather than sending noise to STT.
			frames, activeTotal = nil, 0
			consecutiveActive, consecutiveInactive = 0, 0
			started = false
			g.pre = g.pre[:0]
			continue
		}
	}
}

// finish pads one residual frame at a real utterance boundary per the M0 contract.
// Complete leftover frames stay queued for the next listen.
func (g *gate) finish(frames [][]byte, active int) utterance {
	if len(g.residual) > 0 {
		padded := make([]byte, domain.FrameBytes)
		copy(padded, g.residual)
		g.residual = nil
		if frameActive(padded, g.cfg.speechThreshold) {
			active++
		}
		frames = append(frames, padded)
	}
	return utterance{frames: frames, active: active}
}

// appendChunk adds one validated canonical chunk and reframes it into complete frames.
func (g *gate) appendChunk(data []byte) error {
	g.residual = append(g.residual, data...)
	for len(g.residual) >= domain.FrameBytes {
		frame := make([]byte, domain.FrameBytes)
		copy(frame, g.residual[:domain.FrameBytes])
		g.pending = append(g.pending, frame)
		g.residual = g.residual[domain.FrameBytes:]
	}
	if len(g.residual) == 0 {
		g.residual = nil
	}
	return nil
}

// nextFrame returns one queued frame, or ok=false when more inbound audio is needed.
func (g *gate) nextFrame() ([]byte, bool, error) {
	if len(g.pending) == 0 {
		return nil, false, nil
	}
	frame := g.pending[0]
	g.pending = g.pending[1:]
	if len(frame) != domain.FrameBytes {
		return nil, false, errors.New("agent: internal framing produced a non-canonical frame")
	}
	return frame, true, nil
}

// frameActive applies the deliberately simple RMS >= threshold rule.
func frameActive(frame []byte, threshold float64) bool {
	return frameRMS(frame) >= threshold
}

// frameRMS computes the root mean square of signed PCM16LE samples.
func frameRMS(frame []byte) float64 {
	samples := len(frame) / 2
	if samples == 0 {
		return 0
	}
	var sumSquares float64
	for i := 0; i < samples; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[2*i:])))
		sumSquares += s * s
	}
	return math.Sqrt(sumSquares / float64(samples))
}
