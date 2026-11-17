package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/artemidius/insteadly/internal/core/ports"
)

// playbackState is per-call playback ownership: a monotonically increasing generation and
// the TTS stream currently generating audio. A barge/clear bumps the generation so every
// late chunk from the old generation is discarded by the drainer.
type playbackState struct {
	gen     atomic.Uint64
	tracker speechTracker
	markers markerSeq
}

func (p *playbackState) generation() uint64 { return p.gen.Load() }

func (p *playbackState) bump() uint64 { return p.gen.Add(1) }

// markerSeq produces bounded opaque playback IDs that are unique within one call.
type markerSeq struct{ n atomic.Uint64 }

func (m *markerSeq) next() string { return fmt.Sprintf("m%d", m.n.Add(1)) }

// interruptKind distinguishes a real user barge from confirmed hold audio.
type interruptKind int

const (
	interruptBarge interruptKind = iota
	interruptHold
)

// interruption captures the conservative result of canceling an assistant turn: only
// whole segments whose provider marks arrived before the clear count as confirmed speech.
type interruption struct {
	kind      interruptKind
	confirmed []string
	pending   *utteranceResult
}

// interruptPlayback performs the bounded concurrent Abort + ClearAudio sequence. It must be
// called after the per-turn context was canceled and before any replacement playback. A
// clear failure is operational: speaking over stale playback or claiming barge-in worked is
// forbidden.
func (a *Agent) interruptPlayback(ctx context.Context, call AudioSession, pb *playbackState) error {
	iCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.InterruptionTimeout)
	defer cancel()

	var wg sync.WaitGroup
	var abortErr, clearErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		abortErr = pb.tracker.abort(iCtx)
	}()
	go func() {
		defer wg.Done()
		clearErr = call.ClearAudio(iCtx)
	}()
	wg.Wait()

	if clearErr != nil {
		if ctx.Err() != nil {
			return ErrCallEnded
		}
		return fmt.Errorf("clear audio: %w", clearErr)
	}
	if abortErr != nil {
		return fmt.Errorf("abort synthesis: %w", abortErr)
	}
	// Our Abort is deliberately non-blocking past its bound; a missed bound is still an
	// operational failure unless the whole call was canceled concurrently.
	if iCtx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("interruption bound exceeded: %w", iCtx.Err())
	}
	return nil
}

// waitTurnRollback waits for the canceled dialogue turn to finish its rollback. The adapter
// returns from Wait on cancellation quickly; a bounded independent context prevents a
// wedged adapter from blocking the whole interruption sequence.
func (a *Agent) waitTurnRollback(ctx context.Context, turn ports.AssistantTurn) {
	if turn == nil {
		return
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.InterruptionTimeout)
	defer cancel()
	_, _ = turn.Wait(rollbackCtx)
}

// reconcileInterruptedTurn rolls the dialogue history back to the triggering user/tool
// messages and records only provider-confirmed assistant text.
func (a *Agent) reconcileInterruptedTurn(ctx context.Context, session ports.DialogueSession, turn ports.AssistantTurn, confirmed string) error {
	a.waitTurnRollback(ctx, turn)
	if session == nil {
		return nil
	}
	iCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.InterruptionTimeout)
	defer cancel()
	if err := session.Interrupt(iCtx, confirmed); err != nil {
		return fmt.Errorf("dialogue interrupt: %w", err)
	}
	return nil
}

// isTerminalTurnError reports whether an error from a turn operation belongs to call
// cancellation rather than an operational failure.
func isTerminalTurnError(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrCallEnded) || errors.Is(err, errTerminalDuringTurn)
}
