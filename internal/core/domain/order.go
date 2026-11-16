package domain

import (
	"errors"
	"fmt"
	"time"
)

// OrderStatus is where an order sits in the spine state machine.
type OrderStatus string

const (
	OrderCreated   OrderStatus = "created"
	OrderPaid      OrderStatus = "paid"
	OrderScheduled OrderStatus = "scheduled"
	OrderDialing   OrderStatus = "dialing"
	OrderRinging   OrderStatus = "ringing"
	OrderInCall    OrderStatus = "in_call"
	OrderFinished  OrderStatus = "finished"
)

// OutcomeKind is the structured result of a finished order.
type OutcomeKind string

const (
	OutcomeBooked         OutcomeKind = "booked"
	OutcomeCallbackNeeded OutcomeKind = "callback_needed"
	OutcomeFailed         OutcomeKind = "failed"
	OutcomeAborted        OutcomeKind = "aborted"
)

// Order is the spine every subsystem hangs off (DESIGN.md §2.1.1).
type Order struct {
	ID        OrderID         `json:"id"`
	Status    OrderStatus     `json:"status"`
	Clinic    Clinic          `json:"clinic"`
	Patient   Patient         `json:"patient"`
	Outcome   *Outcome        `json:"outcome"`
	Recording *RecordingFlags `json:"recording"` // per-order overrides; nil = global defaults
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Attempts  int             `json:"attempts"`
}

// Clinic is the practice to be called.
type Clinic struct {
	Name  string `json:"name"`
	Phone string `json:"phone"` // E.164-ish; free text for now, validated against a directory in C5
}

// Patient carries the details the agent needs to negotiate the appointment.
type Patient struct {
	FullName        string      `json:"full_name"`
	Phone           string      `json:"phone"`
	BirthDate       string      `json:"birth_date"` // YYYY-MM-DD, optional for now
	InsuranceNumber string      `json:"insurance_number"`
	Reason          string      `json:"reason"`
	PreferredSlots  []TimeRange `json:"preferred_slots"`
}

// TimeRange is one window the patient would accept.
type TimeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RecordingFlags are per-order policy overrides (DESIGN.md §3.4). nil pointer = use global.
type RecordingFlags struct {
	Recording  *bool `json:"recording"`
	Transcript *bool `json:"transcript"`
	Audio      *bool `json:"audio"`
}

// Outcome is the structured result stored when an order finishes.
type Outcome struct {
	Kind       OutcomeKind `json:"kind"`
	Detail     string      `json:"detail"`
	BookedSlot *BookedSlot `json:"booked_slot,omitempty"`
	CallbackAt *time.Time  `json:"callback_at,omitempty"`
	FailReason string      `json:"fail_reason,omitempty"`
}

// BookedSlot is the appointment the clinic confirmed.
type BookedSlot struct {
	DateTime time.Time `json:"date_time"`
	Doctor   string    `json:"doctor"`
	Practice string    `json:"practice"`
}

var (
	// ErrInvalidTransition is returned when the state machine rejects a status change.
	ErrInvalidTransition = errors.New("invalid status transition")
	// ErrInvalidOutcome is returned when an outcome is missing required data.
	ErrInvalidOutcome = errors.New("invalid outcome")
)

// transitions is the only allowed forward movement between statuses. "finished" is NOT
// reachable here — only via Finish, which enforces a valid Outcome. Retry-from-failed is
// a C5 scheduler concern and must not be added to this table.
var transitions = map[OrderStatus]map[OrderStatus]struct{}{
	OrderCreated:   {OrderPaid: {}},
	OrderPaid:      {OrderScheduled: {}},
	OrderScheduled: {OrderDialing: {}},
	OrderDialing:   {OrderRinging: {}},
	OrderRinging:   {OrderInCall: {}},
	OrderInCall:    {},
}

// Transition moves the order to next if the state machine allows it, and stamps UpdatedAt.
func (o *Order) Transition(next OrderStatus) error {
	targets, ok := transitions[o.Status]
	if !ok {
		return fmt.Errorf("%w: from %q", ErrInvalidTransition, o.Status)
	}
	if _, ok := targets[next]; !ok {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidTransition, o.Status, next)
	}
	o.Status = next
	o.UpdatedAt = time.Now()
	return nil
}

// Finish moves the order to the terminal "finished" status with a validated Outcome.
func (o *Order) Finish(out Outcome) error {
	if o.Status == OrderFinished {
		return fmt.Errorf("%w: already finished", ErrInvalidTransition)
	}
	if err := validateOutcome(out); err != nil {
		return err
	}
	o.Outcome = &out
	o.Status = OrderFinished
	o.UpdatedAt = time.Now()
	return nil
}

// Terminal reports whether the order has reached its final status.
func (o Order) Terminal() bool { return o.Status == OrderFinished }

func validateOutcome(out Outcome) error {
	switch out.Kind {
	case OutcomeBooked:
		if out.BookedSlot == nil || out.BookedSlot.DateTime.IsZero() {
			return fmt.Errorf("%w: booked requires booked_slot", ErrInvalidOutcome)
		}
	case OutcomeCallbackNeeded:
		if out.CallbackAt == nil {
			return fmt.Errorf("%w: callback_needed requires callback_at", ErrInvalidOutcome)
		}
	case OutcomeFailed:
		if out.FailReason == "" {
			return fmt.Errorf("%w: failed requires fail_reason", ErrInvalidOutcome)
		}
	case OutcomeAborted:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidOutcome, out.Kind)
	}
	return nil
}
