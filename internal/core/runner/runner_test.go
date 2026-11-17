package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func preload(s *fakeSession, events ...domain.CallEvent) {
	for _, ev := range events {
		s.send(ev)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	base := Config{
		Telephony: &fakeTelephony{},
		Orders:    newFakeStore(),
		Agent:     newFakeAgent(domain.Outcome{}, nil),
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.CallTimeout = -1 },
		func(c *Config) { c.CleanupTimeout = -1 },
		func(c *Config) { c.ReadinessTimeout = -1 },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Error("negative timeout accepted")
		}
	}
	r, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.log == nil {
		t.Error("logger not defaulted")
	}
	if r.cfg.CallTimeout != defaultCallTimeout || r.cfg.CleanupTimeout != defaultCleanupTimeout ||
		r.cfg.ReadinessTimeout != defaultReadinessTimeout {
		t.Errorf("defaults = %+v", r.cfg)
	}
}

func TestRunNotFound(t *testing.T) {
	store := newFakeStore()
	telephony := &fakeTelephony{session: newFakeSession()}
	r := testRunner(t, store, telephony, newFakeAgent(bookedOutcome(), nil))
	_, err := r.Run(context.Background(), domain.NewOrderID())
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if telephony.dials() != 0 {
		t.Error("dialed a missing order")
	}
}

func TestRunOnlyScheduledAccepted(t *testing.T) {
	for _, status := range []domain.OrderStatus{domain.OrderCreated, domain.OrderPaid, domain.OrderDialing, domain.OrderInCall, domain.OrderFinished} {
		t.Run(string(status), func(t *testing.T) {
			order := testOrder()
			order.Status = status
			store := newFakeStore(order)
			session := newFakeSession()
			telephony := &fakeTelephony{session: session}
			r := testRunner(t, store, telephony, newFakeAgent(bookedOutcome(), nil))
			got, err := r.Run(context.Background(), order.ID)
			if !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("err = %v, want ErrInvalidTransition", err)
			}
			if got.Status != status {
				t.Errorf("returned status = %q", got.Status)
			}
			if telephony.dials() != 0 {
				t.Error("dialed a non-scheduled order")
			}
			if got.Attempts != 0 {
				t.Error("incremented attempts for a non-scheduled order")
			}
		})
	}
}

func TestRunAtomicClaimRejectsConcurrentDuplicate(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	block := make(chan struct{})
	telephony := &fakeTelephony{session: session, blockDial: block}
	ag := newFakeAgent(bookedOutcome(), nil)
	r := testRunner(t, store, telephony, ag)

	first := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		first <- err
	}()
	deadline := time.Now().Add(time.Second)
	for telephony.dials() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if telephony.dials() != 1 {
		t.Fatal("first run did not claim/dial")
	}

	if _, err := r.Run(context.Background(), order.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second run err = %v, want ErrInvalidTransition", err)
	}
	close(block)
	if err := <-first; err != nil {
		t.Fatalf("first run: %v", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", stored.Attempts)
	}
	if telephony.dials() != 1 {
		t.Errorf("dials = %d, want 1", telephony.dials())
	}
}

func TestRunDialErrorFinalizesFailed(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	dialErr := errors.New("provider down")
	telephony := &fakeTelephony{session: session, dialErr: dialErr}
	r := testRunner(t, store, telephony, newFakeAgent(bookedOutcome(), nil))

	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, dialErr) {
		t.Fatalf("err = %v, want dial error", err)
	}
	if got.Status != domain.OrderFinished || got.Outcome == nil ||
		got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final order = %+v", got)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d", got.Attempts)
	}
	if session.hangups() != 0 {
		t.Error("hangup called without a session")
	}
}

func TestRunDialExternalCancelIsAborted(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	block := make(chan struct{})
	telephony := &fakeTelephony{session: newFakeSession(), blockDial: block}
	r := testRunner(t, store, telephony, newFakeAgent(bookedOutcome(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, order.ID)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for telephony.dials() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Status != domain.OrderFinished || stored.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestRunDialContextStaysActiveForSessionLifetime(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	telephony := &fakeTelephony{session: session}
	ag := newFakeAgent(domain.Outcome{Kind: domain.OutcomeAborted, Detail: "aborted"}, nil)
	var sawCanceled bool
	ag.onRun = func(ctx context.Context, _ *fakeSession) {
		sawCanceled = ctx.Err() != nil
	}
	r := testRunner(t, store, telephony, ag)
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawCanceled {
		t.Error("call context was canceled after setup")
	}
}

func TestRunStatusHistoryAndDuplicateProgress(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session,
		domain.CallEvent{Kind: domain.EventDialing},
		domain.CallEvent{Kind: domain.EventRinging},
		domain.CallEvent{Kind: domain.EventRinging},
		domain.CallEvent{Kind: domain.EventAnswered},
		domain.CallEvent{Kind: domain.EventAnswered},
	)
	telephony := &fakeTelephony{session: session}
	ag := newFakeAgent(domain.Outcome{Kind: domain.OutcomeAborted, Detail: "aborted"}, nil)
	r := testRunner(t, store, telephony, ag)
	got, err := r.Run(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != domain.OrderFinished || got.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("final = %+v", got)
	}
	want := []domain.OrderStatus{domain.OrderDialing, domain.OrderRinging, domain.OrderInCall, domain.OrderFinished}
	gotHistory := store.statusHistory()
	if len(gotHistory) != len(want) {
		t.Fatalf("history = %v, want %v", gotHistory, want)
	}
	for i := range want {
		if gotHistory[i] != want[i] {
			t.Errorf("history[%d] = %q, want %q", i, gotHistory[i], want[i])
		}
	}
	if ag.runCount() != 1 {
		t.Errorf("agent runs = %d, want 1", ag.runCount())
	}
	if session.hangups() != 1 {
		t.Errorf("hangups = %d, want 1", session.hangups())
	}
}

func TestRunDTMFIgnored(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session,
		domain.CallEvent{Kind: domain.EventDialing},
		domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"},
		domain.CallEvent{Kind: domain.EventRinging},
		domain.CallEvent{Kind: domain.EventAnswered},
	)
	r := testRunner(t, store, &fakeTelephony{session: session}, newFakeAgent(bookedOutcome(), nil))
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunSkippedAndRegressedEventsFail(t *testing.T) {
	cases := map[string][]domain.CallEvent{
		"answered without ringing": {{Kind: domain.EventAnswered}},
		"ringing then dialing":     {{Kind: domain.EventRinging}, {Kind: domain.EventDialing}},
		"unknown kind":             {{Kind: "bogus"}},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			order := testOrder()
			store := newFakeStore(order)
			session := newFakeSession()
			preload(session, events...)
			ag := newFakeAgent(bookedOutcome(), nil)
			r := testRunner(t, store, &fakeTelephony{session: session}, ag)
			got, err := r.Run(context.Background(), order.ID)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, errProviderContract) {
				t.Errorf("err = %v", err)
			}
			if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
				t.Errorf("final = %+v", got)
			}
			if ag.runCount() != 0 {
				t.Error("agent started after an invalid event")
			}
			if session.hangups() != 1 {
				t.Errorf("hangups = %d, want 1", session.hangups())
			}
		})
	}
}

func TestRunMissingEndedIsProviderFailure(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(false)...)
	session.closeWithoutEnd()
	r := testRunner(t, store, &fakeTelephony{session: session}, newFakeAgent(bookedOutcome(), nil))
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, errMissingEnded) {
		t.Fatalf("err = %v, want errMissingEnded", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
	if session.hangups() != 1 {
		t.Errorf("hangups = %d", session.hangups())
	}
}

func TestRunAgentMissingTerminal(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	session.autoEndOnHangup = false
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, agent.ErrCallEnded)
	ag.onRun = func(context.Context, *fakeSession) { session.closeWithoutEnd() }
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, errMissingEnded) {
		t.Fatalf("err = %v, want errMissingEnded", err)
	}
	if got.Outcome == nil || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
}

func TestRunStreamReadyVariants(t *testing.T) {
	t.Run("no hook starts immediately", func(t *testing.T) {
		order := testOrder()
		store := newFakeStore(order)
		session := newFakeSession()
		preload(session, progress(true)...)
		ag := newFakeAgent(bookedOutcome(), nil)
		r := testRunner(t, store, &fakeTelephony{session: session}, ag)
		if _, err := r.Run(context.Background(), order.ID); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ag.runCount() != 1 {
			t.Errorf("agent runs = %d", ag.runCount())
		}
	})

	t.Run("hook already closed", func(t *testing.T) {
		order := testOrder()
		store := newFakeStore(order)
		session := newFakeSession()
		preload(session, progress(true)...)
		ready := make(chan struct{})
		close(ready)
		ag := newFakeAgent(bookedOutcome(), nil)
		r := testRunner(t, store, &fakeTelephony{session: session, ready: ready}, ag)
		if _, err := r.Run(context.Background(), order.ID); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ag.runCount() != 1 {
			t.Errorf("agent runs = %d", ag.runCount())
		}
	})

	t.Run("hook closes later", func(t *testing.T) {
		order := testOrder()
		store := newFakeStore(order)
		session := newFakeSession()
		preload(session, progress(true)...)
		ready := make(chan struct{})
		go func() {
			time.Sleep(10 * time.Millisecond)
			close(ready)
		}()
		ag := newFakeAgent(bookedOutcome(), nil)
		r := testRunner(t, store, &fakeTelephony{session: session, ready: ready}, ag)
		if _, err := r.Run(context.Background(), order.ID); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ag.runCount() != 1 {
			t.Errorf("agent runs = %d", ag.runCount())
		}
	})

	t.Run("readiness timeout", func(t *testing.T) {
		order := testOrder()
		store := newFakeStore(order)
		session := newFakeSession()
		preload(session, progress(true)...)
		ready := make(chan struct{})
		ag := newFakeAgent(bookedOutcome(), nil)
		r, err := New(Config{
			Telephony:        &fakeTelephony{session: session, ready: ready},
			Orders:           store,
			Agent:            ag,
			CleanupTimeout:   100 * time.Millisecond,
			ReadinessTimeout: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := r.Run(context.Background(), order.ID)
		if !errors.Is(err, errReadinessTimeout) {
			t.Fatalf("err = %v, want errReadinessTimeout", err)
		}
		if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
			t.Fatalf("final = %+v", got)
		}
		if ag.runCount() != 0 {
			t.Error("agent started before readiness")
		}
		if session.hangups() != 1 {
			t.Errorf("hangups = %d", session.hangups())
		}
	})

	t.Run("call ends before ready", func(t *testing.T) {
		order := testOrder()
		store := newFakeStore(order)
		session := newFakeSession()
		preload(session, progress(true)...)
		session.end(domain.EndBusy)
		ready := make(chan struct{})
		ag := newFakeAgent(bookedOutcome(), nil)
		r := testRunner(t, store, &fakeTelephony{session: session, ready: ready}, ag)
		got, err := r.Run(context.Background(), order.ID)
		if err != nil {
			t.Fatalf("expected nil error for busy, got %v", err)
		}
		if got.Outcome == nil || got.Outcome.FailReason != "busy" {
			t.Fatalf("final = %+v", got)
		}
		if ag.runCount() != 0 {
			t.Error("agent started after the call ended")
		}
	})
}

func TestRunAgentStartedOnce(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{Kind: domain.OutcomeAborted, Detail: "aborted"}, nil)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ag.runCount() != 1 {
		t.Errorf("agent runs = %d, want 1", ag.runCount())
	}
}

func TestRunExternalCancellationAborts(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, nil)
	ag.block = true
	ag.started = make(chan struct{})
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, order.ID)
		done <- err
	}()
	<-ag.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Outcome == nil || stored.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("stored = %+v", stored)
	}
	if session.hangups() != 1 {
		t.Errorf("hangups = %d", session.hangups())
	}
}

func TestRunInternalCallTimeout(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, nil)
	ag.block = true
	ag.started = make(chan struct{})
	r, err := New(Config{
		Telephony:      &fakeTelephony{session: session},
		Orders:         store,
		Agent:          ag,
		CallTimeout:    25 * time.Millisecond,
		CleanupTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Run(context.Background(), order.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Outcome == nil || stored.Outcome.Kind != domain.OutcomeFailed || stored.Outcome.FailReason != "other" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestRunAgentCallEndedUsesTerminal(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, agent.ErrCallEnded)
	ag.session = session
	ag.onRun = func(_ context.Context, s *fakeSession) { s.end(domain.EndBusy) }
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("expected nil error for busy, got %v", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "busy" {
		t.Fatalf("final = %+v", got)
	}
}

func TestRunAgentNoOutcomeIsFailure(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	r := testRunner(t, store, &fakeTelephony{session: session}, newFakeAgent(domain.Outcome{}, nil))
	got, err := r.Run(context.Background(), order.ID)
	if err == nil {
		t.Fatal("expected an error for a nil outcome")
	}
	if got.Outcome == nil || got.Outcome.FailReason != "other" {
		t.Fatalf("final = %+v", got)
	}
}

func TestRunAgentOperationalError(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	sentinel := errors.New("tts failed")
	r := testRunner(t, store, &fakeTelephony{session: session}, newFakeAgent(domain.Outcome{}, sentinel))
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
}

func TestRunClaimLostAfterGetWrapsInvalidTransition(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	// Simulate another runner claiming between Get and the update callback.
	store.beforeUpdate = func(o *domain.Order) {
		store.beforeUpdate = nil
		o.Status = domain.OrderDialing
	}
	telephony := &fakeTelephony{session: newFakeSession()}
	ag := newFakeAgent(bookedOutcome(), nil)
	r := testRunner(t, store, telephony, ag)
	_, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if telephony.dials() != 0 {
		t.Error("dialed after losing the claim race")
	}
	if ag.runCount() != 0 {
		t.Error("agent started after losing the claim race")
	}
}

func TestRunReadinessTimeoutNotResetByEvents(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	session.autoEndOnHangup = false // keep the channel open for the event ticker
	preload(session, progress(true)...)
	ready := make(chan struct{}) // never closes
	ag := newFakeAgent(bookedOutcome(), nil)
	r, err := New(Config{
		Telephony:        &fakeTelephony{session: session, ready: ready},
		Orders:           store,
		Agent:            ag,
		CleanupTimeout:   100 * time.Millisecond,
		ReadinessTimeout: 50 * time.Millisecond,
		CallTimeout:      3 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case session.events <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"}:
				default:
				}
			}
		}
	}()
	start := time.Now()
	_, err = r.Run(context.Background(), order.ID)
	elapsed := time.Since(start)
	close(stop)
	<-tickerDone
	if !errors.Is(err, errReadinessTimeout) {
		t.Fatalf("err = %v, want errReadinessTimeout", err)
	}
	if elapsed > time.Second {
		t.Fatalf("readiness wait took %v; a recurring event reset the 50ms bound", elapsed)
	}
	if ag.runCount() != 0 {
		t.Error("agent started before readiness")
	}
}
