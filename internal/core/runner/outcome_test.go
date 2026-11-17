package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestFallbackOutcomeTable(t *testing.T) {
	cases := []struct {
		name       string
		event      domain.CallEvent
		have       bool
		provider   bool
		wantKind   domain.OutcomeKind
		wantReason string
		wantErr    error
	}{
		{name: "aborted", event: domain.CallEvent{EndReason: domain.EndAborted}, have: true, wantKind: domain.OutcomeAborted},
		{name: "busy", event: domain.CallEvent{EndReason: domain.EndBusy}, have: true, wantKind: domain.OutcomeFailed, wantReason: "busy"},
		{name: "no answer", event: domain.CallEvent{EndReason: domain.EndNoAnswer}, have: true, wantKind: domain.OutcomeFailed, wantReason: "no_answer"},
		{name: "rejected", event: domain.CallEvent{EndReason: domain.EndRejected}, have: true, wantKind: domain.OutcomeFailed, wantReason: "rejected"},
		{name: "ivr dead end", event: domain.CallEvent{EndReason: domain.EndIVRDeadEnd}, have: true, wantKind: domain.OutcomeFailed, wantReason: "ivr_dead_end"},
		{name: "completed without outcome", event: domain.CallEvent{EndReason: domain.EndCompleted}, have: true, wantKind: domain.OutcomeFailed, wantReason: "other"},
		{name: "provider error", event: domain.CallEvent{EndReason: domain.EndError}, have: true, wantKind: domain.OutcomeFailed, wantReason: "error", wantErr: errProviderFailure},
		{name: "error event wins", event: domain.CallEvent{EndReason: domain.EndAborted}, have: true, provider: true, wantKind: domain.OutcomeFailed, wantReason: "error", wantErr: errProviderFailure},
		{name: "missing ended", have: false, wantKind: domain.OutcomeFailed, wantReason: "error", wantErr: errMissingEnded},
		{name: "unknown reason", event: domain.CallEvent{EndReason: "weird"}, have: true, wantKind: domain.OutcomeFailed, wantReason: "other"},
		{name: "empty reason", event: domain.CallEvent{}, have: true, wantKind: domain.OutcomeFailed, wantReason: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fallbackOutcome(tc.event, tc.have, tc.provider)
			if got.Kind != tc.wantKind || got.FailReason != tc.wantReason {
				t.Errorf("outcome = %+v, want kind=%s reason=%s", got, tc.wantKind, tc.wantReason)
			}
			if got.Detail == "" {
				t.Error("fallback detail is blank")
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("unexpected error %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRunRemoteOutcomeTable(t *testing.T) {
	cases := []struct {
		name       string
		events     []domain.CallEvent
		wantKind   domain.OutcomeKind
		wantReason string
		wantErr    error
	}{
		{name: "busy", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndBusy}}, wantKind: domain.OutcomeFailed, wantReason: "busy"},
		{name: "no answer", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndNoAnswer}}, wantKind: domain.OutcomeFailed, wantReason: "no_answer"},
		{name: "rejected", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndRejected}}, wantKind: domain.OutcomeFailed, wantReason: "rejected"},
		{name: "ivr", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndIVRDeadEnd}}, wantKind: domain.OutcomeFailed, wantReason: "ivr_dead_end"},
		{name: "aborted", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndAborted}}, wantKind: domain.OutcomeAborted},
		{name: "completed", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndCompleted}}, wantKind: domain.OutcomeFailed, wantReason: "other"},
		{name: "error", events: []domain.CallEvent{{Kind: domain.EventEnded, EndReason: domain.EndError}}, wantKind: domain.OutcomeFailed, wantReason: "error", wantErr: errProviderFailure},
		{name: "error event", events: []domain.CallEvent{{Kind: domain.EventError}, {Kind: domain.EventEnded, EndReason: domain.EndAborted}}, wantKind: domain.OutcomeFailed, wantReason: "error", wantErr: errProviderFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order := testOrder()
			store := newFakeStore(order)
			session := newFakeSession()
			preload(session, append(progress(false), tc.events...)...)
			ag := newFakeAgent(bookedOutcome(), nil)
			r := testRunner(t, store, &fakeTelephony{session: session}, ag)
			got, err := r.Run(context.Background(), order.ID)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got.Status != domain.OrderFinished || got.Outcome == nil {
				t.Fatalf("final = %+v", got)
			}
			if got.Outcome.Kind != tc.wantKind || got.Outcome.FailReason != tc.wantReason {
				t.Errorf("outcome = %+v", got.Outcome)
			}
			if got.Outcome.Detail == "" {
				t.Error("detail is blank")
			}
			if got.Attempts != 1 {
				t.Errorf("attempts = %d", got.Attempts)
			}
			if session.hangups() != 1 {
				t.Errorf("hangups = %d, want 1", session.hangups())
			}
			if store.finishCount != 1 {
				t.Errorf("finish count = %d, want 1", store.finishCount)
			}
			if ag.runCount() != 0 {
				t.Error("agent started for a call that never answered")
			}
		})
	}
}

func TestRunBookedOutcomeWinsOverHangupEndAborted(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(bookedOutcome(), nil)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", got.Outcome)
	}
	if got.Outcome.BookedSlot == nil {
		t.Fatal("booked slot dropped")
	}
	if store.finishCount != 1 || session.hangups() != 1 {
		t.Errorf("finish/hangup = %d/%d, want 1/1", store.finishCount, session.hangups())
	}
}

func TestRunBookedValidationErrorFailsAttempt(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, domain.ErrInvalidOutcome)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, domain.ErrInvalidOutcome) {
		t.Fatalf("err = %v, want ErrInvalidOutcome", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
	if session.hangups() != 1 || store.finishCount != 1 {
		t.Errorf("hangup/finish = %d/%d", session.hangups(), store.finishCount)
	}
}

func TestRunProviderErrorDuringAgent(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, agent.ErrCallEnded)
	ag.session = session
	ag.onRun = func(_ context.Context, s *fakeSession) {
		s.send(domain.CallEvent{Kind: domain.EventError})
		s.end(domain.EndAborted)
	}
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, errProviderFailure) {
		t.Fatalf("err = %v, want errProviderFailure", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
}

func TestRunFinalizeFailureIsNotHidden(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	finalizeErr := errors.New("storage gone")
	store.failOnFinish = true
	store.finishErr = finalizeErr
	ag := newFakeAgent(bookedOutcome(), nil)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	got, err := r.Run(context.Background(), order.ID)
	if !errors.Is(err, finalizeErr) {
		t.Fatalf("err = %v, want finalize error", err)
	}
	if got.Status == domain.OrderFinished {
		t.Error("runner claimed a finished order that was not persisted")
	}
	if got.Status != domain.OrderInCall {
		t.Errorf("last known status = %q, want in_call", got.Status)
	}
}

func TestRunOneFinishOneHangup(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session, progress(true)...)
	ag := newFakeAgent(bookedOutcome(), nil)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stored, _ := store.Get(context.Background(), order.ID)
	if stored.Status != domain.OrderFinished {
		t.Errorf("status = %q", stored.Status)
	}
	if store.finishCount != 1 {
		t.Errorf("Finish calls = %d, want 1", store.finishCount)
	}
	if session.hangups() != 1 {
		t.Errorf("Hangup calls = %d, want 1", session.hangups())
	}
}
