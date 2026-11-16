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
