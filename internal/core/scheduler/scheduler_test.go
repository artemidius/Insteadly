package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// --- fakes ----------------------------------------------------------------------

type fakeLease struct {
	owner string
	until time.Time
}

type fakeStore struct {
	mu     sync.Mutex
	orders map[domain.OrderID]domain.Order
	leases map[domain.OrderID]fakeLease
	clock  *FakeClock
}

func newFakeStore(clock *FakeClock) *fakeStore {
	return &fakeStore{
		orders: make(map[domain.OrderID]domain.Order),
		leases: make(map[domain.OrderID]fakeLease),
		clock:  clock,
	}
}

func (s *fakeStore) put(o domain.Order) domain.Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.clock.Now()
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = o.CreatedAt
	}
	s.orders[o.ID] = o
	return o
}

func (s *fakeStore) Create(_ context.Context, o domain.Order) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	if _, ok := s.orders[o.ID]; ok {
		return domain.Order{}, ports.ErrConflict
	}
	o.Status = domain.OrderCreated
	s.orders[o.ID] = o
	return o, nil
}

func (s *fakeStore) Get(_ context.Context, id domain.OrderID) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	return o, nil
}

func (s *fakeStore) List(context.Context) ([]domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out, nil
}

func (s *fakeStore) Update(_ context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	if err := fn(&o); err != nil {
		return domain.Order{}, err
	}
	s.orders[id] = o
	return o, nil
}

func (s *fakeStore) ListEligible(_ context.Context, now time.Time, limit int) ([]domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Order, 0, len(s.orders))
	for id, o := range s.orders {
		if !eligibleAt(o, now) {
			continue
		}
		if l, ok := s.leases[id]; ok && l.until.After(now) {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].NextAttemptAt, out[j].NextAttemptAt
		switch {
		case a == nil && b == nil:
			return out[i].ID < out[j].ID
		case a == nil:
			return true
		case b == nil:
			return false
		case a.Equal(*b):
			return out[i].ID < out[j].ID
		default:
			return a.Before(*b)
		}
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) Claim(_ context.Context, id domain.OrderID, owner string, leaseUntil time.Time) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, fmt.Errorf("claim %s: %w", id, ports.ErrNotEligible)
	}
	now := s.clock.Now()
	if !eligibleAt(o, now) {
		return domain.Order{}, fmt.Errorf("claim %s: %w", id, ports.ErrNotEligible)
	}
	if l, ok := s.leases[id]; ok && l.until.After(now) {
		return domain.Order{}, fmt.Errorf("claim %s: %w", id, ports.ErrNotEligible)
	}
	s.leases[id] = fakeLease{owner: owner, until: leaseUntil}
	return o, nil
}

func (s *fakeStore) Release(_ context.Context, id domain.OrderID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[id]; ok && l.owner == owner {
		delete(s.leases, id)
	}
	return nil
}

func (s *fakeStore) snapshot(id domain.OrderID) (domain.Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	return o, ok
}

func (s *fakeStore) leaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.leases)
}

// eligibleAt is the test-side mirror of the store eligibility rule.
func eligibleAt(o domain.Order, now time.Time) bool {
	switch o.Status {
	case domain.OrderPaid:
		return true
	case domain.OrderScheduled:
		return o.NextAttemptAt == nil || !o.NextAttemptAt.After(now)
	default:
		return false
	}
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []domain.OrderID
	fn    func(ctx context.Context, id domain.OrderID) (domain.Order, error)
}

func (r *fakeRunner) Run(ctx context.Context, id domain.OrderID) (domain.Order, error) {
	r.mu.Lock()
	r.calls = append(r.calls, id)
	fn := r.fn
	r.mu.Unlock()
	if fn == nil {
		return domain.Order{}, errors.New("fake runner has no behaviour")
	}
	return fn(ctx, id)
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// finishingRunner emulates the runner's persisted outcome: increment Attempts and finish
// with outcome. The optional block channel holds the attempt open for concurrency tests.
func finishingRunner(store *fakeStore, clock Clock, outcome domain.Outcome, block <-chan struct{}) *fakeRunner {
	return &fakeRunner{fn: func(ctx context.Context, id domain.OrderID) (domain.Order, error) {
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				return domain.Order{}, ctx.Err()
			}
		}
		return store.Update(ctx, id, func(o *domain.Order) error {
			if o.Status != domain.OrderScheduled {
				return fmt.Errorf("status %q is not scheduled", o.Status)
			}
			o.Attempts++
			o.Status = domain.OrderFinished
			o.Outcome = &outcome
			o.UpdatedAt = clock.Now()
			return nil
		})
	}}
}

// --- harness --------------------------------------------------------------------

func allDay() Windows {
	ws := make(Windows, 0, 7)
	for d := time.Sunday; d <= time.Saturday; d++ {
		ws = append(ws, Window{Dow: d, Start: "00:00", End: "23:59"})
	}
	return ws
}

func testClock(t *testing.T) *FakeClock {
	t.Helper()
	return NewFakeClock(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)) // Tuesday
}

func testScheduler(t *testing.T, store *fakeStore, runner Runner, mutate func(*Config)) *Scheduler {
	t.Helper()
	cfg := Config{
		Store:           store,
		Orders:          store,
		Runner:          runner,
		Clock:           store.clock,
		Location:        time.UTC,
		Windows:         allDay(),
		Tick:            time.Hour,
		MaxAttempts:     3,
		MaxConcurrent:   2,
		Lease:           time.Minute,
		BusyBackoff:     15 * time.Minute,
		NoAnswerBackoff: 30 * time.Minute,
		ErrorBackoff:    5 * time.Minute,
		WorkerID:        "test-worker",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	return s
}

func runOnceAndWait(t *testing.T, s *Scheduler) {
	t.Helper()
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func dueScheduled(clock *FakeClock, attempts int) domain.Order {
	due := clock.Now().Add(-time.Minute)
	return domain.Order{Status: domain.OrderScheduled, Attempts: attempts, NextAttemptAt: &due}
}

// --- tests ----------------------------------------------------------------------

func TestPaidOrderScheduledAndRun(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	order := store.put(domain.Order{Status: domain.OrderPaid})
	hub := events.NewHub(16)
	defer hub.Close()
	sub, unsub, err := hub.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	runner := finishingRunner(store, clock, domain.Outcome{
		Kind:       domain.OutcomeBooked,
		BookedSlot: &domain.BookedSlot{DateTime: clock.Now().Add(24 * time.Hour)},
	}, nil)
	s := testScheduler(t, store, runner, func(c *Config) { c.Events = hub })
	runOnceAndWait(t, s)

	final, ok := store.snapshot(order.ID)
	if !ok {
		t.Fatal("order disappeared")
	}
	if final.Status != domain.OrderFinished || final.Attempts != 1 {
		t.Fatalf("final = %+v, want finished with 1 attempt", final)
	}
	if final.Outcome == nil || final.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v, want booked", final.Outcome)
	}
	if runner.callCount() != 1 {
		t.Errorf("runner calls = %d, want 1", runner.callCount())
	}
	if store.leaseCount() != 0 {
		t.Errorf("lease not released: %d", store.leaseCount())
	}

	select {
	case ev := <-sub:
		if ev.Status != domain.OrderScheduled || ev.Attempts != 0 || ev.OrderID != order.ID {
			t.Errorf("scheduled event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no scheduled event published")
	}
}

func TestRetryBackoffByReason(t *testing.T) {
	cases := map[string]struct {
		outcome domain.Outcome
		want    time.Duration
	}{
		"busy":         {domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "busy"}, 15 * time.Minute},
		"no_answer":    {domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "no_answer"}, 30 * time.Minute},
		"ivr_dead_end": {domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "ivr_dead_end"}, 30 * time.Minute},
		"error":        {domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "error"}, 5 * time.Minute},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clock := testClock(t)
			store := newFakeStore(clock)
			order := store.put(dueScheduled(clock, 0))
			runner := finishingRunner(store, clock, tc.outcome, nil)
			s := testScheduler(t, store, runner, nil)
			runOnceAndWait(t, s)

			got, _ := store.snapshot(order.ID)
			if got.Status != domain.OrderScheduled {
				t.Fatalf("status = %q, want scheduled after requeue", got.Status)
			}
			if got.Outcome != nil {
				t.Errorf("outcome = %+v, want nil after requeue", got.Outcome)
			}
			want := clock.Now().Add(tc.want)
			if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(want) {
				t.Errorf("next_attempt_at = %v, want %v", got.NextAttemptAt, want)
			}
			if len(got.AttemptHistory) != 1 {
				t.Fatalf("history = %d, want 1", len(got.AttemptHistory))
			}
			if got.AttemptHistory[0].Number != 1 || got.AttemptHistory[0].Outcome.FailReason != tc.outcome.FailReason {
				t.Errorf("history record = %+v", got.AttemptHistory[0])
			}
			if got.Attempts != 1 {
				t.Errorf("attempts = %d, want 1", got.Attempts)
			}
			if store.leaseCount() != 0 {
				t.Errorf("lease not released")
			}
		})
	}
}

func TestRetryBackoffScalesWithAttemptNumber(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	order := store.put(dueScheduled(clock, 1))
	runner := finishingRunner(store, clock, domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "busy"}, nil)
	s := testScheduler(t, store, runner, nil)
	runOnceAndWait(t, s)

	got, _ := store.snapshot(order.ID)
	want := clock.Now().Add(30 * time.Minute) // 15m * attempt 2
	if got.Status != domain.OrderScheduled || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(want) {
		t.Fatalf("order = %+v, want requeued at %v", got, want)
	}
}

func TestTerminalOutcomesAreNotRequeued(t *testing.T) {
	cases := map[string]domain.Outcome{
		"booked":   {Kind: domain.OutcomeBooked, BookedSlot: &domain.BookedSlot{DateTime: time.Date(2027, 1, 1, 9, 0, 0, 0, time.UTC)}},
		"aborted":  {Kind: domain.OutcomeAborted},
		"rejected": {Kind: domain.OutcomeFailed, FailReason: "rejected"},
		"other":    {Kind: domain.OutcomeFailed, FailReason: "other"},
		"unknown":  {Kind: domain.OutcomeFailed, FailReason: "teleported"},
	}
	for name, outcome := range cases {
		t.Run(name, func(t *testing.T) {
			clock := testClock(t)
			store := newFakeStore(clock)
			order := store.put(dueScheduled(clock, 0))
			runner := finishingRunner(store, clock, outcome, nil)
			s := testScheduler(t, store, runner, nil)
			runOnceAndWait(t, s)

			got, _ := store.snapshot(order.ID)
			if got.Status != domain.OrderFinished {
				t.Fatalf("status = %q, want finished", got.Status)
			}
			if got.Outcome == nil || got.Outcome.Kind != outcome.Kind {
				t.Errorf("outcome = %+v, want unchanged", got.Outcome)
			}
			if len(got.AttemptHistory) != 0 {
				t.Errorf("terminal outcome appended history: %+v", got.AttemptHistory)
			}
		})
	}
}

func TestCallbackRequeueUsesCallbackAt(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	order := store.put(dueScheduled(clock, 0))
	callbackAt := clock.Now().Add(2 * time.Hour)
	outcome := domain.Outcome{Kind: domain.OutcomeCallbackNeeded, CallbackAt: &callbackAt}
	runner := finishingRunner(store, clock, outcome, nil)
	s := testScheduler(t, store, runner, nil)
	runOnceAndWait(t, s)

	got, _ := store.snapshot(order.ID)
	if got.Status != domain.OrderScheduled {
		t.Fatalf("status = %q, want scheduled", got.Status)
	}
	if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(callbackAt) {
		t.Errorf("next_attempt_at = %v, want callback_at %v", got.NextAttemptAt, callbackAt)
	}
	if got.Outcome != nil {
		t.Errorf("outcome = %+v, want nil", got.Outcome)
	}
	if len(got.AttemptHistory) != 1 || got.AttemptHistory[0].Outcome.Kind != domain.OutcomeCallbackNeeded {
		t.Errorf("history = %+v", got.AttemptHistory)
	}
}

func TestCallbackInPastIsClampedToWindow(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	order := store.put(dueScheduled(clock, 0))
	past := clock.Now().Add(-time.Hour)
	outcome := domain.Outcome{Kind: domain.OutcomeCallbackNeeded, CallbackAt: &past}
	runner := finishingRunner(store, clock, outcome, nil)
	s := testScheduler(t, store, runner, nil)
	runOnceAndWait(t, s)

	got, _ := store.snapshot(order.ID)
	if got.Status != domain.OrderScheduled || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(clock.Now()) {
		t.Fatalf("order = %+v, want requeued at now", got)
	}
}

func TestAttemptsCapMarksExhausted(t *testing.T) {
	for name, outcome := range map[string]domain.Outcome{
		"callback": {Kind: domain.OutcomeCallbackNeeded},
		"busy":     {Kind: domain.OutcomeFailed, FailReason: "busy"},
	} {
		t.Run(name, func(t *testing.T) {
			clock := testClock(t)
			store := newFakeStore(clock)
			order := store.put(dueScheduled(clock, 2)) // the next attempt is #3
			if name == "callback" {
				at := clock.Now().Add(time.Hour)
				outcome.CallbackAt = &at
			}
			runner := finishingRunner(store, clock, outcome, nil)
			s := testScheduler(t, store, runner, nil)
			runOnceAndWait(t, s)

			got, _ := store.snapshot(order.ID)
			if got.Status != domain.OrderFinished {
				t.Fatalf("status = %q, want finished", got.Status)
			}
			if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != FailReasonAttemptsExhausted {
				t.Fatalf("outcome = %+v, want failed/attempts_exhausted", got.Outcome)
			}
			if got.Attempts != 3 {
				t.Errorf("attempts = %d, want 3", got.Attempts)
			}
			if len(got.AttemptHistory) != 0 {
				t.Errorf("exhausted order appended history")
			}
		})
	}
}

func TestOutsideWindowIsNeverClaimed(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)) // Tuesday 20:00
	store := newFakeStore(clock)
	order := store.put(domain.Order{Status: domain.OrderPaid})
	runner := finishingRunner(store, clock, domain.Outcome{Kind: domain.OutcomeAborted}, nil)
	s := testScheduler(t, store, runner, func(c *Config) {
		c.Windows = Windows{{Dow: time.Tuesday, Start: "08:00", End: "18:00"}}
	})
	runOnceAndWait(t, s)

	if runner.callCount() != 0 {
		t.Errorf("runner called %d times outside the window", runner.callCount())
	}
	got, _ := store.snapshot(order.ID)
	if got.Status != domain.OrderPaid {
		t.Errorf("status = %q, want paid (untouched)", got.Status)
	}

	// An empty window set is a permanent quiet period.
	clock.Set(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC))
	store2 := newFakeStore(clock)
	order2 := store2.put(domain.Order{Status: domain.OrderPaid})
	runner2 := finishingRunner(store2, clock, domain.Outcome{Kind: domain.OutcomeAborted}, nil)
	s2 := testScheduler(t, store2, runner2, func(c *Config) { c.Windows = nil })
	runOnceAndWait(t, s2)
	if runner2.callCount() != 0 {
		t.Errorf("runner called %d times with empty windows", runner2.callCount())
	}
	if got, _ := store2.snapshot(order2.ID); got.Status != domain.OrderPaid {
		t.Errorf("empty-window order status = %q", got.Status)
	}
}

func TestConcurrencyCapRespected(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	for i := 0; i < 5; i++ {
		store.put(domain.Order{Status: domain.OrderPaid})
	}
	release := make(chan struct{})
	started := make(chan struct{}, 5)
	var mu sync.Mutex
	active, maxActive := 0, 0
	runner := &fakeRunner{fn: func(ctx context.Context, id domain.OrderID) (domain.Order, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		active--
		mu.Unlock()
		return domain.Order{}, errors.New("rejected for test")
	}}
	s := testScheduler(t, store, runner, func(c *Config) {
		c.MaxConcurrent = 2
		c.Windows = allDay()
	})
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("runner did not start two attempts")
		}
	}
	// A second scan while both slots are busy must not start anything.
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := runner.callCount(); got != 2 {
		t.Errorf("runner calls = %d while at cap, want 2", got)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive > 2 {
		t.Errorf("max concurrent = %d, want <= 2", maxActive)
	}
	if store.leaseCount() != 0 {
		t.Errorf("leases left behind: %d", store.leaseCount())
	}
}

func TestLeaseReleasedWhenRunnerRejects(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	order := store.put(dueScheduled(clock, 0))
	runner := &fakeRunner{fn: func(context.Context, domain.OrderID) (domain.Order, error) {
		return domain.Order{}, errors.New("runner rejected the attempt")
	}}
	s := testScheduler(t, store, runner, nil)
	runOnceAndWait(t, s)

	if store.leaseCount() != 0 {
		t.Fatalf("lease was not released after a rejected run")
	}
	if _, err := store.Claim(context.Background(), order.ID, "other-worker", clock.Now().Add(time.Minute)); err != nil {
		t.Fatalf("order is no longer claimable: %v", err)
	}
	got, _ := store.snapshot(order.ID)
	if got.Status != domain.OrderScheduled {
		t.Errorf("status = %q, want scheduled (runner rejection does not finish)", got.Status)
	}
}

func TestCloseCancelsInFlightRuns(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	store.put(domain.Order{Status: domain.OrderPaid})
	sawCancel := make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	runner := &fakeRunner{fn: func(ctx context.Context, _ domain.OrderID) (domain.Order, error) {
		started <- struct{}{}
		<-ctx.Done()
		sawCancel <- struct{}{}
		return domain.Order{}, ctx.Err()
	}}
	s := testScheduler(t, store, runner, nil)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Fatal("in-flight run did not observe cancellation")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce after Close: %v", err)
	}
}

func TestStartLoopAndClose(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	store.put(domain.Order{Status: domain.OrderPaid})
	started := make(chan struct{}, 1)
	runner := &fakeRunner{fn: func(ctx context.Context, _ domain.OrderID) (domain.Order, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return domain.Order{}, ctx.Err()
	}}
	s := testScheduler(t, store, runner, func(c *Config) { c.Tick = time.Millisecond })
	s.Start(context.Background())
	s.Start(context.Background()) // idempotent
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not start an attempt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted missing dependencies")
	}
	clock := testClock(t)
	store := newFakeStore(clock)
	runner := &fakeRunner{}
	if _, err := New(Config{
		Store: store, Orders: store, Runner: runner,
		Clock: clock, Windows: allDay(), MaxAttempts: -1,
	}); err == nil {
		t.Fatal("New accepted negative max attempts")
	}
	if _, err := New(Config{
		Store: store, Orders: store, Runner: runner,
		Clock: clock, Windows: allDay(), Lease: -1,
	}); err == nil {
		t.Fatal("New accepted a negative lease")
	}
	// Zero values take documented defaults.
	s, err := New(Config{Store: store, Orders: store, Runner: runner, Clock: clock, Windows: allDay(), Logger: nil})
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if s.cfg.MaxAttempts != defaultMaxAttempts || s.cfg.MaxConcurrent != defaultMaxConcurrent ||
		s.cfg.Tick != defaultTick || s.cfg.Lease != defaultLease {
		t.Errorf("defaults not applied: %+v", s.cfg)
	}
	if s.cfg.WorkerID == "" {
		t.Error("worker id is empty")
	}
}

func TestWaitReturnsImmediatelyWhenIdle(t *testing.T) {
	clock := testClock(t)
	store := newFakeStore(clock)
	s := testScheduler(t, store, &fakeRunner{}, nil)
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
