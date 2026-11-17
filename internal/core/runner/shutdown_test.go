package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestCloseRejectsNewRuns(t *testing.T) {
	store := newFakeStore()
	telephony := &fakeTelephony{session: newFakeSession()}
	r := testRunner(t, store, telephony, newFakeAgent(bookedOutcome(), nil))
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Run(context.Background(), domain.NewOrderID()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if telephony.dials() != 0 {
		t.Error("new Run after Close reached Dial")
	}
}

func TestCloseRepeatedIsSafe(t *testing.T) {
	r := testRunner(t, newFakeStore(), &fakeTelephony{session: newFakeSession()}, newFakeAgent(bookedOutcome(), nil))
	for i := 0; i < 3; i++ {
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

func TestCloseCancelsActiveCall(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, nil)
	ag.block = true
	ag.started = make(chan struct{})
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	<-ag.started

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := <-done
	if !errors.Is(err, ErrClosed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want ErrClosed + context.Canceled", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Outcome == nil || stored.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("stored = %+v", stored)
	}
	if session.hangups() != 1 {
		t.Errorf("hangups = %d, want 1", session.hangups())
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.hangupCtxErr != nil {
		t.Errorf("hangup used a canceled context: %v", session.hangupCtxErr)
	}
}

func TestCloseDeadlineThenCompletes(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	release := make(chan struct{})
	ag := newFakeAgent(bookedOutcome(), nil)
	ag.block = true
	ag.ignoreCtx = true
	ag.release = release
	ag.started = make(chan struct{})
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	<-ag.started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err = %v, want DeadlineExceeded", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseConcurrentRunsAndCloses(t *testing.T) {
	orders := make([]domain.Order, 4)
	for i := range orders {
		orders[i] = testOrder()
	}
	store := newFakeStore(orders...)
	telephony := &fakeTelephony{sessionFactory: func() *fakeSession { return newFakeSession() }}
	ag := newFakeAgent(domain.Outcome{}, nil)
	ag.block = true
	r := testRunner(t, store, telephony, ag)

	var runWG sync.WaitGroup
	errCh := make(chan error, len(orders))
	for _, order := range orders {
		runWG.Add(1)
		go func(id domain.OrderID) {
			defer runWG.Done()
			_, err := r.Run(context.Background(), id)
			errCh <- err
		}(order.ID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for telephony.dials() < len(orders) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	var closeWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			if err := r.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	closeWG.Wait()
	runWG.Wait()
	close(errCh)
	for err := range errCh {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("run err = %v, want ErrClosed", err)
		}
	}
	if telephony.dials() != len(orders) {
		t.Errorf("dials = %d, want %d", telephony.dials(), len(orders))
	}
	if _, err := r.Run(context.Background(), domain.NewOrderID()); !errors.Is(err, ErrClosed) {
		t.Errorf("post-close err = %v", err)
	}
}
