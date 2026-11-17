package domain

import (
	"errors"
	"testing"
	"time"
)

var allStatuses = []OrderStatus{
	OrderCreated, OrderPaid, OrderScheduled, OrderDialing, OrderRinging, OrderInCall, OrderFinished,
}

func TestTransitionLegal(t *testing.T) {
	legal := []struct{ from, to OrderStatus }{
		{OrderCreated, OrderPaid},
		{OrderPaid, OrderScheduled},
		{OrderScheduled, OrderDialing},
		{OrderDialing, OrderRinging},
		{OrderRinging, OrderInCall},
	}
	for _, tc := range legal {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			o := Order{ID: NewOrderID(), Status: tc.from}
			if err := o.Transition(tc.to); err != nil {
				t.Fatalf("Transition(%q) from %q: unexpected error: %v", tc.to, tc.from, err)
			}
			if o.Status != tc.to {
				t.Errorf("status = %q, want %q", o.Status, tc.to)
			}
			if o.UpdatedAt.IsZero() {
				t.Error("UpdatedAt was not stamped")
			}
		})
	}
}

func TestTransitionIllegal(t *testing.T) {
	legal := map[[2]OrderStatus]bool{
		{OrderCreated, OrderPaid}:      true,
		{OrderPaid, OrderScheduled}:    true,
		{OrderScheduled, OrderDialing}: true,
		{OrderDialing, OrderRinging}:   true,
		{OrderRinging, OrderInCall}:    true,
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			if legal[[2]OrderStatus{from, to}] {
				continue
			}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				o := Order{ID: NewOrderID(), Status: from}
				err := o.Transition(to)
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("Transition(%q) from %q: err = %v, want ErrInvalidTransition", to, from, err)
				}
				if o.Status != from {
					t.Errorf("status changed to %q despite error", o.Status)
				}
			})
		}
	}
}

func TestTransitionToFinishedRejected(t *testing.T) {
	o := Order{Status: OrderInCall}
	if err := o.Transition(OrderFinished); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestFinishValid(t *testing.T) {
	when := time.Date(2026, 11, 20, 9, 15, 0, 0, time.UTC)
	cases := map[string]Outcome{
		"booked":          {Kind: OutcomeBooked, BookedSlot: &BookedSlot{DateTime: when, Doctor: "Dr. Huber"}},
		"callback_needed": {Kind: OutcomeCallbackNeeded, CallbackAt: &when},
		"failed":          {Kind: OutcomeFailed, FailReason: "busy"},
		"aborted":         {Kind: OutcomeAborted},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			o := Order{ID: NewOrderID(), Status: OrderInCall}
			if err := o.Finish(out); err != nil {
				t.Fatalf("Finish: unexpected error: %v", err)
			}
			if o.Status != OrderFinished {
				t.Errorf("status = %q, want %q", o.Status, OrderFinished)
			}
			if o.Outcome == nil || o.Outcome.Kind != out.Kind {
				t.Errorf("outcome = %+v, want kind %q", o.Outcome, out.Kind)
			}
			if o.UpdatedAt.IsZero() {
				t.Error("UpdatedAt was not stamped")
			}
			if !o.Terminal() {
				t.Error("Terminal() = false after Finish")
			}
		})
	}
}

func TestFinishInvalid(t *testing.T) {
	cases := map[string]Outcome{
		"booked without slot":   {Kind: OutcomeBooked},
		"booked zero datetime":  {Kind: OutcomeBooked, BookedSlot: &BookedSlot{Doctor: "Dr. X"}},
		"booked empty slot":     {Kind: OutcomeBooked, BookedSlot: &BookedSlot{}},
		"callback without time": {Kind: OutcomeCallbackNeeded},
		"failed without reason": {Kind: OutcomeFailed},
		"unknown kind":          {Kind: OutcomeKind("teleported")},
		"empty kind":            {Kind: OutcomeKind("")},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			o := Order{ID: NewOrderID(), Status: OrderInCall}
			err := o.Finish(out)
			if !errors.Is(err, ErrInvalidOutcome) {
				t.Fatalf("Finish: err = %v, want ErrInvalidOutcome", err)
			}
			if o.Status != OrderInCall {
				t.Errorf("status = %q, want unchanged %q", o.Status, OrderInCall)
			}
			if o.Outcome != nil {
				t.Errorf("outcome was set despite error: %+v", o.Outcome)
			}
		})
	}
}

func TestFinishTwice(t *testing.T) {
	o := Order{ID: NewOrderID(), Status: OrderInCall}
	if err := o.Finish(Outcome{Kind: OutcomeAborted}); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	err := o.Finish(Outcome{Kind: OutcomeAborted})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second Finish: err = %v, want ErrInvalidTransition", err)
	}
}

func TestTerminal(t *testing.T) {
	for _, st := range allStatuses {
		want := st == OrderFinished
		if got := (Order{Status: st}).Terminal(); got != want {
			t.Errorf("Terminal() for %q = %v, want %v", st, got, want)
		}
	}
}

func TestScheduleFromPaid(t *testing.T) {
	next := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	o := Order{ID: NewOrderID(), Status: OrderPaid}
	if err := o.Schedule(&next); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if o.Status != OrderScheduled {
		t.Errorf("status = %q, want scheduled", o.Status)
	}
	if o.NextAttemptAt == nil || !o.NextAttemptAt.Equal(next) {
		t.Errorf("next_attempt_at = %v, want %v", o.NextAttemptAt, next)
	}
	if o.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

func TestScheduleNilMeansImmediate(t *testing.T) {
	o := Order{ID: NewOrderID(), Status: OrderPaid}
	if err := o.Schedule(nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if o.Status != OrderScheduled || o.NextAttemptAt != nil {
		t.Errorf("order = %+v, want scheduled with nil next_attempt_at", o)
	}
}

func TestScheduleRejected(t *testing.T) {
	for _, from := range []OrderStatus{OrderCreated, OrderScheduled, OrderDialing, OrderFinished} {
		t.Run(string(from), func(t *testing.T) {
			o := Order{ID: NewOrderID(), Status: from}
			err := o.Schedule(nil)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Schedule from %q: err = %v, want ErrInvalidTransition", from, err)
			}
			if o.Status != from {
				t.Errorf("status changed to %q despite error", o.Status)
			}
		})
	}
}

func callbackOutcome(when time.Time) Outcome {
	return Outcome{Kind: OutcomeCallbackNeeded, Detail: "call back", CallbackAt: &when}
}

func TestRequeueCallbackPreservesHistory(t *testing.T) {
	next := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	stamp := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	out := callbackOutcome(next)
	o := Order{ID: NewOrderID(), Status: OrderFinished, Outcome: &out, Attempts: 1, UpdatedAt: stamp}

	if err := o.Requeue(&next); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if o.Status != OrderScheduled {
		t.Errorf("status = %q, want scheduled", o.Status)
	}
	if o.Outcome != nil {
		t.Errorf("outcome = %+v, want nil after requeue", o.Outcome)
	}
	if o.NextAttemptAt == nil || !o.NextAttemptAt.Equal(next) {
		t.Errorf("next_attempt_at = %v, want %v", o.NextAttemptAt, next)
	}
	if len(o.AttemptHistory) != 1 {
		t.Fatalf("history = %d, want 1", len(o.AttemptHistory))
	}
	rec := o.AttemptHistory[0]
	if rec.Number != 1 || rec.Outcome.Kind != OutcomeCallbackNeeded {
		t.Errorf("history record = %+v", rec)
	}
	if !rec.StartedAt.Equal(stamp) {
		t.Errorf("started_at = %v, want the pre-requeue UpdatedAt %v", rec.StartedAt, stamp)
	}
	if rec.EndedAt.IsZero() {
		t.Error("ended_at was not stamped")
	}
}

func TestRequeueRetryableFailed(t *testing.T) {
	failed := Outcome{Kind: OutcomeFailed, FailReason: "busy"}
	o := Order{ID: NewOrderID(), Status: OrderFinished, Outcome: &failed, Attempts: 2}
	if err := o.Requeue(nil); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if o.Status != OrderScheduled || len(o.AttemptHistory) != 1 || o.AttemptHistory[0].Number != 2 {
		t.Errorf("order = %+v", o)
	}
}

func TestRequeueRejected(t *testing.T) {
	future := time.Date(2027, 1, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		order Order
		want  error
	}{
		{
			name:  "not finished",
			order: Order{Status: OrderInCall, Outcome: &Outcome{Kind: OutcomeCallbackNeeded, CallbackAt: &future}},
			want:  ErrInvalidTransition,
		},
		{
			name:  "finished without outcome",
			order: Order{Status: OrderFinished},
			want:  ErrInvalidTransition,
		},
		{
			name:  "booked",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeBooked, BookedSlot: &BookedSlot{DateTime: future}}},
			want:  ErrInvalidTransition,
		},
		{
			name:  "aborted",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeAborted}},
			want:  ErrInvalidTransition,
		},
		{
			name:  "unknown kind",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeKind("teleported")}},
			want:  ErrInvalidTransition,
		},
		{
			name:  "callback without time",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeCallbackNeeded}},
			want:  ErrInvalidOutcome,
		},
		{
			name:  "callback zero time",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeCallbackNeeded, CallbackAt: &time.Time{}}},
			want:  ErrInvalidOutcome,
		},
		{
			name:  "failed without reason",
			order: Order{Status: OrderFinished, Outcome: &Outcome{Kind: OutcomeFailed}},
			want:  ErrInvalidOutcome,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.order
			before := o.Status
			if err := o.Requeue(nil); !errors.Is(err, tc.want) {
				t.Fatalf("Requeue err = %v, want %v", err, tc.want)
			}
			if o.Status != before || len(o.AttemptHistory) != 0 {
				t.Errorf("order mutated despite error: %+v", o)
			}
		})
	}
}

func TestAttemptHistoryAppendOrdering(t *testing.T) {
	first := Outcome{Kind: OutcomeFailed, FailReason: "busy"}
	o := Order{ID: NewOrderID(), Status: OrderFinished, Outcome: &first, Attempts: 1}
	if err := o.Requeue(nil); err != nil {
		t.Fatalf("Requeue 1: %v", err)
	}
	o.Attempts = 2
	if err := o.Finish(Outcome{Kind: OutcomeFailed, FailReason: "no_answer"}); err != nil {
		t.Fatalf("Finish 2: %v", err)
	}
	if err := o.Requeue(nil); err != nil {
		t.Fatalf("Requeue 2: %v", err)
	}
	if len(o.AttemptHistory) != 2 {
		t.Fatalf("history = %d, want 2", len(o.AttemptHistory))
	}
	if o.AttemptHistory[0].Number != 1 || o.AttemptHistory[1].Number != 2 {
		t.Errorf("history numbers = %d,%d", o.AttemptHistory[0].Number, o.AttemptHistory[1].Number)
	}
	if o.AttemptHistory[0].Outcome.FailReason != "busy" || o.AttemptHistory[1].Outcome.FailReason != "no_answer" {
		t.Errorf("history reasons = %+v", o.AttemptHistory)
	}
}

func TestMarkPaidFromCreated(t *testing.T) {
	when := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	o := Order{ID: NewOrderID(), Status: OrderCreated}
	if err := o.MarkPaid("  cs_test_123  ", when); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if o.Status != OrderPaid {
		t.Errorf("status = %q, want paid", o.Status)
	}
	if o.PaidAt == nil || !o.PaidAt.Equal(when) {
		t.Errorf("paid_at = %v, want %v", o.PaidAt, when)
	}
	if o.PaymentReference == nil || *o.PaymentReference != "cs_test_123" {
		t.Errorf("payment_reference = %v, want trimmed cs_test_123", o.PaymentReference)
	}
	if o.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

func TestMarkPaidZeroTimeUsesNow(t *testing.T) {
	before := time.Now()
	o := Order{ID: NewOrderID(), Status: OrderCreated}
	if err := o.MarkPaid("ref", time.Time{}); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	after := time.Now()
	if o.PaidAt == nil || o.PaidAt.Before(before) || o.PaidAt.After(after) {
		t.Errorf("paid_at = %v, want between %v and %v", o.PaidAt, before, after)
	}
}

func TestMarkPaidIdempotentAfterCreated(t *testing.T) {
	when := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	for _, status := range []OrderStatus{OrderPaid, OrderScheduled, OrderDialing, OrderRinging, OrderInCall, OrderFinished} {
		t.Run(string(status), func(t *testing.T) {
			o := Order{ID: NewOrderID(), Status: status}
			if err := o.MarkPaid("cs_replay", when); err != nil {
				t.Fatalf("MarkPaid: %v", err)
			}
			if o.Status != status {
				t.Errorf("status = %q, want unchanged %q", o.Status, status)
			}
			if o.PaidAt != nil || o.PaymentReference != nil {
				t.Errorf("replay mutated payment fields: paid_at=%v ref=%v", o.PaidAt, o.PaymentReference)
			}
		})
	}
}

func TestMarkPaidDoesNotChangeSpine(t *testing.T) {
	// MarkPaid is not a shortcut through the state machine: created may still only move
	// to paid, and skipping steps remains illegal.
	o := Order{ID: NewOrderID(), Status: OrderCreated}
	if err := o.Transition(OrderScheduled); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("created -> scheduled err = %v, want ErrInvalidTransition", err)
	}
	if err := o.MarkPaid("ref", time.Now()); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if err := o.Transition(OrderPaid); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("paid -> paid err = %v, want ErrInvalidTransition", err)
	}
}
