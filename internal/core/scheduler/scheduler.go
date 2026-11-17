// Package scheduler drives due orders through one call attempt at a time. It lists
// eligible orders from a ports.SchedulerStore, lease-claims them, schedules paid orders,
// and hands each order to Runner.Run — the only place a scheduled order becomes dialing.
//
// The scheduler itself never moves an order to dialing; it owns eligibility, opening
// windows, bounded concurrency, retry backoff, and history-preserving callback requeue.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	defaultTick            = 5 * time.Second
	defaultMaxAttempts     = 3
	defaultMaxConcurrent   = 8
	defaultLease           = 2 * time.Minute
	defaultBusyBackoff     = 15 * time.Minute
	defaultNoAnswerBackoff = 30 * time.Minute
	defaultErrorBackoff    = 5 * time.Minute
	defaultLocation        = "Europe/Vienna"
	cleanupTimeout         = 5 * time.Second

	// FailReasonAttemptsExhausted is the terminal reason recorded when a retryable
	// outcome cannot be requeued because the attempt cap was reached.
	FailReasonAttemptsExhausted = "attempts_exhausted"
)

// Runner is the slice of runner.Runner the scheduler needs. Runner.Run remains the
// sole single-attempt, scheduled→dialing boundary.
type Runner interface {
	Run(ctx context.Context, id domain.OrderID) (domain.Order, error)
}

// Config configures one scheduler. Zero values take the documented defaults.
type Config struct {
	Store  ports.SchedulerStore
	Orders ports.OrderStore
	Runner Runner
	Events events.Publisher // optional; nil disables event publication
	Clock  Clock            // optional; defaults to SystemClock
	// Location is the timezone all window math uses. Defaults to Europe/Vienna.
	Location *time.Location
	Windows  Windows

	Tick            time.Duration
	MaxAttempts     int
	MaxConcurrent   int
	Lease           time.Duration
	BusyBackoff     time.Duration
	NoAnswerBackoff time.Duration
	ErrorBackoff    time.Duration

	// WorkerID identifies this process in a lease. Defaults to insteadly-<pid>.
	WorkerID string
	Logger   *slog.Logger
}

// Scheduler is the in-process orchestration loop. Start it once; Close stops it and
// waits for in-flight attempts.
type Scheduler struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	loopDone chan struct{}
	inflight int
	idle     chan struct{}
	runs     map[*runHandle]struct{}
}

type runHandle struct {
	cancel context.CancelFunc
}

// New validates the wiring and applies defaults. It performs no I/O and starts nothing.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Orders == nil || cfg.Runner == nil {
		return nil, errors.New("scheduler: store, orders, and runner are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Location == nil {
		loc, err := time.LoadLocation(defaultLocation)
		if err != nil {
			return nil, fmt.Errorf("scheduler: load location %s: %w", defaultLocation, err)
		}
		cfg.Location = loc
	}
	applyDuration := func(name string, value *time.Duration, def time.Duration) error {
		if *value < 0 {
			return fmt.Errorf("scheduler: %s must not be negative", name)
		}
		if *value == 0 {
			*value = def
		}
		return nil
	}
	for _, d := range []struct {
		name  string
		value *time.Duration
		def   time.Duration
	}{
		{"tick", &cfg.Tick, defaultTick},
		{"lease", &cfg.Lease, defaultLease},
		{"busy backoff", &cfg.BusyBackoff, defaultBusyBackoff},
		{"no-answer backoff", &cfg.NoAnswerBackoff, defaultNoAnswerBackoff},
		{"error backoff", &cfg.ErrorBackoff, defaultErrorBackoff},
	} {
		if err := applyDuration(d.name, d.value, d.def); err != nil {
			return nil, err
		}
	}
	if cfg.MaxAttempts < 0 || cfg.MaxConcurrent < 0 {
		return nil, errors.New("scheduler: max attempts and max concurrent must not be negative")
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("insteadly-%d", os.Getpid())
	}

	idle := make(chan struct{})
	close(idle)
	return &Scheduler{
		cfg:      cfg,
		log:      cfg.Logger,
		loopDone: make(chan struct{}),
		idle:     idle,
		runs:     make(map[*runHandle]struct{}),
	}, nil
}

// Start launches the scan loop. It is idempotent and a no-op after Close.
func (s *Scheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	go s.loop(loopCtx)
}

// loop scans on the configured tick until its context is canceled.
func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.loopDone)
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil && !s.isClosed() {
				s.log.Warn("scheduler: scan failed", "error", err)
			}
		}
	}
}

// RunOnce performs one eligibility scan and launches at most the free concurrency slots.
// The loop calls it on every tick; tests drive it directly under a fake clock.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if s == nil || s.isClosed() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.cfg.Clock.Now()
	// Outside the opening windows nothing is even claimed. An empty window set means
	// there is never an allowed time (the config validates at least one window, but the
	// scheduler itself stays explicit).
	if !s.cfg.Windows.Allows(now, s.cfg.Location) {
		return nil
	}

	s.mu.Lock()
	free := s.cfg.MaxConcurrent - s.inflight
	s.mu.Unlock()
	if free <= 0 {
		return nil
	}

	due, err := s.cfg.Store.ListEligible(ctx, now, free)
	if err != nil {
		return fmt.Errorf("scheduler: list eligible: %w", err)
	}
	for _, candidate := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.isClosed() {
			return nil
		}
		claimed, err := s.cfg.Store.Claim(ctx, candidate.ID, s.cfg.WorkerID, now.Add(s.cfg.Lease))
		if errors.Is(err, ports.ErrNotEligible) {
			continue
		}
		if err != nil {
			s.log.Warn("scheduler: claim failed", "order_id", candidate.ID, "error", err)
			continue
		}

		prepared := claimed
		if claimed.Status == domain.OrderPaid {
			updated, err := s.cfg.Orders.Update(ctx, claimed.ID, func(o *domain.Order) error {
				return o.Schedule(&now)
			})
			if err != nil {
				s.log.Warn("scheduler: schedule paid order failed", "order_id", claimed.ID, "error", err)
				s.release(ctx, claimed.ID)
				continue
			}
			prepared = updated
			s.publish(updated)
		}
		if prepared.Status != domain.OrderScheduled {
			s.release(ctx, claimed.ID)
			continue
		}

		runCtx, handle, ok := s.beginRun(ctx)
		if !ok {
			s.release(ctx, claimed.ID)
			return nil
		}
		go s.runOne(runCtx, prepared, handle)
	}
	return nil
}

// runOne executes one claimed order and applies the retry/requeue policy. The lease is
// always released on return; the requeued order is immediately claimable again.
func (s *Scheduler) runOne(ctx context.Context, order domain.Order, handle *runHandle) {
	defer s.endRun(handle)
	defer s.release(ctx, order.ID)

	final, err := s.cfg.Runner.Run(ctx, order.ID)
	if err != nil {
		s.log.Info("scheduler: attempt returned an error", "order_id", order.ID, "error", err)
	}
	if final.Status != domain.OrderFinished {
		// The runner may have rejected the attempt (another worker won) or failed to
		// persist. Read back the durable state; never infer a requeue from a guess.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		stored, getErr := s.cfg.Orders.Get(readCtx, order.ID)
		cancel()
		if getErr != nil {
			s.log.Warn("scheduler: read back order failed", "order_id", order.ID, "error", getErr)
			return
		}
		final = stored
	}
	s.log.Info("scheduler: attempt finished",
		"order_id", final.ID,
		"status", final.Status,
		"attempts", final.Attempts,
		"outcome", outcomeKind(final.Outcome),
		"fail_reason", outcomeReason(final.Outcome),
	)
	if final.Status != domain.OrderFinished || final.Outcome == nil {
		return
	}

	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	s.applyPolicy(applyCtx, final)
}

// applyPolicy maps a terminal outcome onto the retry/terminal table. It requeues only
// retryable outcomes under the attempt cap; the runner's Attempts count is authoritative.
func (s *Scheduler) applyPolicy(ctx context.Context, final domain.Order) {
	outcome := *final.Outcome
	if !retryable(outcome) {
		return
	}
	if final.Attempts >= s.cfg.MaxAttempts {
		s.markExhausted(ctx, final)
		return
	}
	next := s.nextAttempt(outcome, final.Attempts, s.cfg.Clock.Now())
	updated, err := s.cfg.Orders.Update(ctx, final.ID, func(o *domain.Order) error {
		return o.Requeue(&next)
	})
	if err != nil {
		s.log.Warn("scheduler: requeue failed", "order_id", final.ID, "error", err)
		return
	}
	s.log.Info("scheduler: order requeued", "order_id", updated.ID, "next_attempt_at", next, "attempts", updated.Attempts)
	s.publish(updated)
}

// markExhausted turns a retryable terminal outcome into failed/attempts_exhausted so an
// exhausted order is visibly terminal rather than looking like it still awaits a callback.
func (s *Scheduler) markExhausted(ctx context.Context, final domain.Order) {
	updated, err := s.cfg.Orders.Update(ctx, final.ID, func(o *domain.Order) error {
		if o.Status != domain.OrderFinished {
			return fmt.Errorf("order is %q, want finished", o.Status)
		}
		o.Outcome = &domain.Outcome{
			Kind:       domain.OutcomeFailed,
			FailReason: FailReasonAttemptsExhausted,
			Detail:     "the attempt limit was reached",
		}
		o.UpdatedAt = s.cfg.Clock.Now()
		return nil
	})
	if err != nil {
		s.log.Warn("scheduler: mark attempts exhausted failed", "order_id", final.ID, "error", err)
		return
	}
	s.log.Info("scheduler: attempts exhausted", "order_id", updated.ID, "attempts", updated.Attempts)
	s.publish(updated)
}

// retryable reports whether the policy table says another attempt is worth making.
// booked, aborted, rejected, other, and unknown reasons are terminal.
func retryable(outcome domain.Outcome) bool {
	switch outcome.Kind {
	case domain.OutcomeCallbackNeeded:
		return true
	case domain.OutcomeFailed:
		switch outcome.FailReason {
		case "busy", "no_answer", "ivr_dead_end", "error":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// nextAttempt computes the next due time: callback_at for callback_needed, otherwise a
// per-class base delay multiplied by the just-completed attempt number (linear backoff),
// advanced to the next opening window.
func (s *Scheduler) nextAttempt(outcome domain.Outcome, attempts int, now time.Time) time.Time {
	var candidate time.Time
	switch outcome.Kind {
	case domain.OutcomeCallbackNeeded:
		candidate = now
		if outcome.CallbackAt != nil && outcome.CallbackAt.After(now) {
			candidate = *outcome.CallbackAt
		}
	case domain.OutcomeFailed:
		base := s.noAnswerBackoff(outcome.FailReason)
		if attempts < 1 {
			attempts = 1
		}
		candidate = now.Add(base * time.Duration(attempts))
	}
	return s.cfg.Windows.Next(candidate, s.cfg.Location)
}

func (s *Scheduler) noAnswerBackoff(reason string) time.Duration {
	switch reason {
	case "busy":
		return s.cfg.BusyBackoff
	case "no_answer", "ivr_dead_end":
		return s.cfg.NoAnswerBackoff
	case "error":
		return s.cfg.ErrorBackoff
	default:
		return s.cfg.ErrorBackoff
	}
}

// Wait blocks until no attempts are in flight or ctx expires. Tests use it after RunOnce;
// Close uses it for shutdown.
func (s *Scheduler) Wait(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.inflight == 0 {
		s.mu.Unlock()
		return nil
	}
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops issuing claims, cancels in-flight attempts, and waits for them. It is
// idempotent and safe before Start.
func (s *Scheduler) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	started := s.started
	cancel := s.cancel
	loopDone := s.loopDone
	for h := range s.runs {
		h.cancel()
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		select {
		case <-loopDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Wait(ctx)
}

// beginRun registers one in-flight attempt and derives a context Close can cancel.
func (s *Scheduler) beginRun(parent context.Context) (context.Context, *runHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.inflight >= s.cfg.MaxConcurrent {
		return nil, nil, false
	}
	runCtx, cancel := context.WithCancel(parent)
	h := &runHandle{cancel: cancel}
	s.runs[h] = struct{}{}
	if s.inflight == 0 {
		s.idle = make(chan struct{})
	}
	s.inflight++
	return runCtx, h, true
}

func (s *Scheduler) endRun(h *runHandle) {
	h.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, h)
	s.inflight--
	if s.inflight == 0 {
		close(s.idle)
	}
}

func (s *Scheduler) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Scheduler) release(ctx context.Context, id domain.OrderID) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.cfg.Store.Release(rctx, id, s.cfg.WorkerID); err != nil {
		s.log.Debug("scheduler: release lease failed", "order_id", id, "error", err)
	}
}

func (s *Scheduler) publish(o domain.Order) {
	if s.cfg.Events == nil {
		return
	}
	_ = s.cfg.Events.Publish(context.Background(), events.OrderEvent{
		OrderID:  o.ID,
		Status:   o.Status,
		Outcome:  o.Outcome,
		Attempts: o.Attempts,
		At:       s.cfg.Clock.Now(),
	})
}

func outcomeKind(outcome *domain.Outcome) string {
	if outcome == nil {
		return ""
	}
	return string(outcome.Kind)
}

func outcomeReason(outcome *domain.Outcome) string {
	if outcome == nil {
		return ""
	}
	return outcome.FailReason
}
