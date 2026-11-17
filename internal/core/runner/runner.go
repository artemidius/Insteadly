// Package runner owns one order's single call attempt: claim, dial, event-driven state
// transitions, agent handoff, hangup, and exactly one terminal persistence.
//
// It depends only on the core ports and the core agent; it never imports an adapter.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/recorder"
)

// ErrClosed is returned by Run after the runner has been closed.
var ErrClosed = errors.New("call runner is closed")

const (
	defaultCallTimeout      = 6 * time.Minute
	defaultCleanupTimeout   = 5 * time.Second
	defaultReadinessTimeout = 20 * time.Second
)

// Config configures one runner. Zero timeouts use the defaults above; negative values
// are rejected. These are constructor/test knobs, not new environment settings.
type Config struct {
	Telephony ports.Telephony
	Orders    ports.OrderStore
	Agent     agent.Engine
	Logger    *slog.Logger

	// Events is optional: when set, the runner publishes one OrderEvent after each
	// persisted status change (dialing, ringing, in_call, finished). A nil publisher
	// is a no-op, so neither tests nor non-orchestrated callers need a hub.
	Events events.Publisher

	// Recorder is optional: when set, the dialed session is wrapped exactly once so the
	// recording flags can capture audio. A nil recorder (the default) is today's
	// behavior, and a recorder whose resolved flags are off returns the session
	// unchanged.
	Recorder recorder.Recorder

	CallTimeout      time.Duration
	CleanupTimeout   time.Duration
	ReadinessTimeout time.Duration
}

// Runner executes synchronous single-attempt calls and bounds shutdown.
type Runner struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	closed    bool
	active    map[*activeRun]struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
}

type activeRun struct {
	cancel context.CancelFunc
}

// New validates the wiring, applies timeout defaults, and performs no I/O.
func New(cfg Config) (*Runner, error) {
	if cfg.Telephony == nil || cfg.Orders == nil || cfg.Agent == nil {
		return nil, errors.New("runner: telephony, orders, and agent are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for name, value := range map[string]time.Duration{
		"call timeout":      cfg.CallTimeout,
		"cleanup timeout":   cfg.CleanupTimeout,
		"readiness timeout": cfg.ReadinessTimeout,
	} {
		if value < 0 {
			return nil, fmt.Errorf("runner: %s must not be negative", name)
		}
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = defaultCallTimeout
	}
	if cfg.CleanupTimeout == 0 {
		cfg.CleanupTimeout = defaultCleanupTimeout
	}
	if cfg.ReadinessTimeout == 0 {
		cfg.ReadinessTimeout = defaultReadinessTimeout
	}
	return &Runner{
		cfg:       cfg,
		log:       cfg.Logger,
		active:    make(map[*activeRun]struct{}),
		closeDone: make(chan struct{}),
	}, nil
}

// Run claims and executes one call attempt. It is synchronous; callers wanting
// concurrency invoke it from their own goroutine.
func (r *Runner) Run(callerCtx context.Context, id domain.OrderID) (domain.Order, error) {
	runCtx, reg, ok := r.register(callerCtx)
	if !ok {
		return domain.Order{}, ErrClosed
	}
	defer r.unregister(reg)

	order, err := r.cfg.Orders.Get(runCtx, id)
	if err != nil {
		return domain.Order{}, fmt.Errorf("runner: get order %s: %w", id, err)
	}
	if order.Status != domain.OrderScheduled {
		return order, fmt.Errorf("runner: order %s: %w: status %q is not scheduled", id, domain.ErrInvalidTransition, order.Status)
	}

	claimed, err := r.cfg.Orders.Update(runCtx, id, func(o *domain.Order) error {
		if o.Status != domain.OrderScheduled {
			return fmt.Errorf("order is %q, want scheduled: %w", o.Status, domain.ErrInvalidTransition)
		}
		if err := o.Transition(domain.OrderDialing); err != nil {
			return err
		}
		o.Attempts++
		return nil
	})
	if err != nil {
		return order, fmt.Errorf("runner: claim order %s: %w", id, err)
	}
	r.publish(runCtx, claimed)

	callCtx, cancelCall := context.WithTimeout(runCtx, r.cfg.CallTimeout)
	defer cancelCall()

	session, err := r.cfg.Telephony.Dial(callCtx, claimed)
	if err != nil {
		runErr := fmt.Errorf("runner: dial order %s: %w", id, err)
		outcome, ctxErr, classified := r.contextAbort(callerCtx, runCtx, callCtx)
		if classified {
			runErr = ctxErr
		} else {
			outcome = failedOutcome("error", "the call could not be placed")
		}
		final, finErr := r.persist(callerCtx, id, outcome, claimed)
		return final, errors.Join(runErr, finErr)
	}

	// The single recording seam: wrap the dialed session once, after a successful Dial
	// and before the pre-agent loop. With no recorder, or flags resolving to no capture,
	// the session is returned unchanged and behavior is byte-for-byte identical.
	if r.cfg.Recorder != nil {
		session = r.cfg.Recorder.Wrap(session, claimed)
	}

	hangup := r.newHangup(callerCtx, session)
	state := &preAgentState{order: claimed}
	events := session.Events()

	var readyCh <-chan struct{}
	if withReady, ok := session.(interface{ StreamReady() <-chan struct{} }); ok {
		readyCh = withReady.StreamReady()
	}

	var readinessTimer *time.Timer
	var readinessDeadline <-chan time.Time

preAgent:
	for {
		if state.answered {
			if readyCh == nil {
				break
			}
			// One fixed deadline for the whole readiness wait: recurring events must not
			// postpone the failure until the overall call deadline.
			if readinessTimer == nil {
				readinessTimer = time.NewTimer(r.cfg.ReadinessTimeout)
				readinessDeadline = readinessTimer.C
				defer readinessTimer.Stop()
			}
			select {
			case <-readyCh:
				break preAgent
			case ev, open := <-events:
				if !open {
					return r.finishWithFailure(callerCtx, id, hangup, state.order, errMissingEnded)
				}
				if err := r.handlePreAgentEvent(runCtx, id, state, ev); err != nil {
					return r.finishWithFailure(callerCtx, id, hangup, state.order, err)
				}
				if outcome, runErr, done := r.preAgentTerminal(state); done {
					hangup()
					final, finErr := r.persist(callerCtx, id, outcome, state.order)
					return final, errors.Join(runErr, finErr)
				}
			case <-callerCtx.Done():
				return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
			case <-runCtx.Done():
				return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
			case <-callCtx.Done():
				return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
			case <-readinessDeadline:
				return r.finishWithFailure(callerCtx, id, hangup, state.order, errReadinessTimeout)
			}
			continue
		}

		select {
		case ev, open := <-events:
			if !open {
				return r.finishWithFailure(callerCtx, id, hangup, state.order, errMissingEnded)
			}
			if err := r.handlePreAgentEvent(runCtx, id, state, ev); err != nil {
				return r.finishWithFailure(callerCtx, id, hangup, state.order, err)
			}
			if outcome, runErr, done := r.preAgentTerminal(state); done {
				hangup()
				final, finErr := r.persist(callerCtx, id, outcome, state.order)
				return final, errors.Join(runErr, finErr)
			}
		case <-callerCtx.Done():
			return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
		case <-runCtx.Done():
			return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
		case <-callCtx.Done():
			return r.finishAbort(callerCtx, runCtx, callCtx, id, hangup, state.order)
		}
	}

	// Agent handoff: from here a single relay goroutine owns the event channel.
	relay := newEventRelay(r.log)
	go relay.run(events)
	// The relay must never outlive Run, including when a broken adapter neither ends the
	// call nor closes the event channel.
	defer relay.shutdown()

	agentOutcome, agentErr := r.cfg.Agent.Run(callCtx, state.order, session, relay.agentChannel(), relay.controlChannel())

	// Always hang up once after a successful Dial, using a context independent of the
	// canceled call, then give the provider a bounded chance to close the event stream.
	hangup()
	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(callerCtx), r.cfg.CleanupTimeout)
	relay.wait(waitCtx)
	cancelWait()
	relay.shutdown()

	// A dropped DTMF/mark is never a silent success: even if the agent raced to a valid
	// outcome, the runner refuses to persist a navigation/reconciliation result after a
	// control relay overflow.
	if cerr := relay.controlError(); cerr != nil {
		final, finErr := r.persist(callerCtx, id, failedOutcome("error", detailControlLost), state.order)
		return final, errors.Join(cerr, finErr)
	}

	terminal, haveTerminal := relay.terminal()
	providerErr := relay.providerError()

	var outcome domain.Outcome
	var runErr error
	switch {
	case agentErr == nil && agentOutcome.Kind == "":
		outcome = failedOutcome("other", "the agent returned no outcome")
		runErr = errors.New("runner: agent returned no outcome")
	case agentErr == nil:
		outcome = agentOutcome
	case errors.Is(agentErr, agent.ErrCallEnded):
		outcome, runErr = fallbackOutcome(terminal, haveTerminal, providerErr)
	default:
		if ctxOutcome, ctxErr, classified := r.contextAbort(callerCtx, runCtx, callCtx); classified {
			outcome, runErr = ctxOutcome, ctxErr
		} else if agent.IsLimit(agentErr) {
			outcome = failedOutcome("other", detailLimit)
			runErr = agentErr
		} else {
			outcome = failedOutcome("error", "the call failed during the conversation")
			runErr = agentErr
		}
	}

	final, finErr := r.persist(callerCtx, id, outcome, state.order)
	return final, errors.Join(runErr, finErr)
}

// preAgentState tracks the runner-owned progress before the agent starts. The runner is
// the only consumer of events in this phase.
type preAgentState struct {
	order        domain.Order
	answered     bool
	providerErr  bool
	terminal     domain.CallEvent
	haveTerminal bool
}

// handlePreAgentEvent validates one progress event, drives the order state machine, and
// records provider failures/terminal reasons. Progress may be duplicated by fakes but a
// regression or skipped required state is a provider-contract error.
func (r *Runner) handlePreAgentEvent(ctx context.Context, id domain.OrderID, st *preAgentState, ev domain.CallEvent) error {
	switch ev.Kind {
	case domain.EventDialing:
		if st.order.Status == domain.OrderDialing {
			return nil // claim already recorded dialing
		}
		return r.invalidEvent(st.order, ev)
	case domain.EventRinging:
		switch st.order.Status {
		case domain.OrderDialing:
			updated, err := r.transition(ctx, id, domain.OrderRinging)
			if err != nil {
				return fmt.Errorf("runner: order %s ringing: %w", id, err)
			}
			st.order = updated
			r.publish(ctx, st.order)
		case domain.OrderRinging:
			// duplicate
		default:
			return r.invalidEvent(st.order, ev)
		}
	case domain.EventAnswered:
		switch st.order.Status {
		case domain.OrderRinging:
			updated, err := r.transition(ctx, id, domain.OrderInCall)
			if err != nil {
				return fmt.Errorf("runner: order %s answered: %w", id, err)
			}
			st.order = updated
			r.publish(ctx, st.order)
			st.answered = true
		case domain.OrderInCall:
			// duplicate answered; never start a second agent
		default:
			return r.invalidEvent(st.order, ev)
		}
	case domain.EventDTMF:
		r.log.Debug("runner: dtmf ignored", "order_id", id, "dtmf_len", len(ev.DTMF))
	case domain.EventPlaybackMark:
		// Playback evidence before the agent starts cannot belong to any core segment.
		return fmt.Errorf("runner: order %s: %w: playback mark before answer", id, errProviderContract)
	case domain.EventError:
		st.providerErr = true
	case domain.EventEnded:
		st.terminal = ev
		st.haveTerminal = true
	default:
		return fmt.Errorf("runner: order %s: %w: unexpected event %q", id, errProviderContract, ev.Kind)
	}
	return nil
}

func (r *Runner) invalidEvent(order domain.Order, ev domain.CallEvent) error {
	return fmt.Errorf("runner: order %s: %w: event %q in status %q", order.ID, domain.ErrInvalidTransition, ev.Kind, order.Status)
}

// preAgentTerminal converts a recorded terminal event into the one fallback outcome. It
// reports done=false when no terminal event has arrived yet.
func (r *Runner) preAgentTerminal(st *preAgentState) (domain.Outcome, error, bool) {
	if !st.haveTerminal {
		return domain.Outcome{}, nil, false
	}
	outcome, err := fallbackOutcome(st.terminal, true, st.providerErr)
	return outcome, err, true
}

// finishWithFailure applies failed/error (or the provided sentinel), hangs up once, and
// persists exactly one terminal outcome.
func (r *Runner) finishWithFailure(ctx context.Context, id domain.OrderID, hangup func(), last domain.Order, cause error) (domain.Order, error) {
	outcome := failedOutcome("error", "the call failed")
	if errors.Is(cause, errProviderContract) || errors.Is(cause, errMissingEnded) {
		outcome = failedOutcome("error", "the call ended unexpectedly")
	}
	hangup()
	final, finErr := r.persist(ctx, id, outcome, last)
	return final, errors.Join(cause, finErr)
}

// finishAbort persists aborted for caller cancellation/shutdown or failed/other for the
// internal runner deadline.
func (r *Runner) finishAbort(callerCtx, runCtx, callCtx context.Context, id domain.OrderID, hangup func(), last domain.Order) (domain.Order, error) {
	outcome, ctxErr, classified := r.contextAbort(callerCtx, runCtx, callCtx)
	if !classified {
		outcome = abortedOutcome()
		ctxErr = context.Canceled
	}
	hangup()
	final, finErr := r.persist(callerCtx, id, outcome, last)
	return final, errors.Join(ctxErr, finErr)
}

// contextAbort classifies a canceled run. It returns classified=false when no context is
// done, so callers keep their operational error.
func (r *Runner) contextAbort(callerCtx, runCtx, callCtx context.Context) (domain.Outcome, error, bool) {
	switch {
	case callerCtx.Err() != nil:
		return abortedOutcome(), callerCtx.Err(), true
	case runCtx.Err() != nil:
		return abortedOutcome(), errors.Join(ErrClosed, context.Canceled), true
	case callCtx.Err() != nil:
		return failedOutcome("other", "the call exceeded its overall deadline"), callCtx.Err(), true
	default:
		return domain.Outcome{}, nil, false
	}
}

func (r *Runner) transition(ctx context.Context, id domain.OrderID, next domain.OrderStatus) (domain.Order, error) {
	return r.cfg.Orders.Update(ctx, id, func(o *domain.Order) error {
		return o.Transition(next)
	})
}

// persist writes the single terminal outcome with a fresh bounded context so caller
// cancellation cannot skip the terminal write. One attempt; no retry loop.
func (r *Runner) persist(ctx context.Context, id domain.OrderID, outcome domain.Outcome, last domain.Order) (domain.Order, error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.CleanupTimeout)
	defer cancel()
	final, err := r.cfg.Orders.Update(fctx, id, func(o *domain.Order) error {
		return o.Finish(outcome)
	})
	if err != nil {
		return last, fmt.Errorf("runner: finalize order %s: %w", id, err)
	}
	r.publish(fctx, final)
	return final, nil
}

// publish emits one optional status event. Event delivery is best-effort: the hub is
// non-blocking and a nil publisher is valid, so a publish failure never fails a call.
func (r *Runner) publish(ctx context.Context, o domain.Order) {
	if r.cfg.Events == nil {
		return
	}
	_ = r.cfg.Events.Publish(ctx, events.OrderEvent{
		OrderID:  o.ID,
		Status:   o.Status,
		Outcome:  o.Outcome,
		Attempts: o.Attempts,
		At:       time.Now(),
	})
}

// newHangup returns a once-only hangup using a context independent of the canceled call.
func (r *Runner) newHangup(ctx context.Context, session ports.CallSession) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.CleanupTimeout)
			defer cancel()
			if err := session.Hangup(cctx); err != nil {
				r.log.Warn("runner: cleanup hangup failed", "error", err)
			}
		})
	}
}

func (r *Runner) register(ctx context.Context) (context.Context, *activeRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, false
	}
	runCtx, cancel := context.WithCancel(ctx)
	reg := &activeRun{cancel: cancel}
	r.active[reg] = struct{}{}
	r.wg.Add(1)
	return runCtx, reg, true
}

func (r *Runner) unregister(reg *activeRun) {
	reg.cancel()
	r.mu.Lock()
	delete(r.active, reg)
	r.mu.Unlock()
	r.wg.Done()
}
