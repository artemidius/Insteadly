package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// orderColumns is the domain column list; lease columns are selected only where the
// scheduler needs them so ordinary reads cannot accidentally expose lease state.
const orderColumns = `id, status, clinic, patient, outcome, recording, created_at, updated_at, attempts, next_attempt_at, attempt_history, user_id, paid_at, payment_reference`

type rowScanner interface {
	Scan(dest ...any) error
}

type orderRow struct {
	id               string
	status           string
	clinic           []byte
	patient          []byte
	outcome          []byte
	recording        []byte
	createdAt        time.Time
	updatedAt        time.Time
	attempts         int32
	nextAttemptAt    *time.Time
	history          []byte
	userID           *string
	paidAt           *time.Time
	paymentReference *string
}

func (r orderRow) order() (domain.Order, error) {
	o := domain.Order{
		ID:               domain.OrderID(r.id),
		Status:           domain.OrderStatus(r.status),
		CreatedAt:        r.createdAt,
		UpdatedAt:        r.updatedAt,
		Attempts:         int(r.attempts),
		NextAttemptAt:    r.nextAttemptAt,
		PaidAt:           r.paidAt,
		PaymentReference: r.paymentReference,
	}
	if r.userID != nil {
		o.UserID = domain.UserID(*r.userID)
	}
	if err := json.Unmarshal(r.clinic, &o.Clinic); err != nil {
		return domain.Order{}, fmt.Errorf("decode clinic: %w", err)
	}
	if err := json.Unmarshal(r.patient, &o.Patient); err != nil {
		return domain.Order{}, fmt.Errorf("decode patient: %w", err)
	}
	if len(r.outcome) > 0 {
		var outcome domain.Outcome
		if err := json.Unmarshal(r.outcome, &outcome); err != nil {
			return domain.Order{}, fmt.Errorf("decode outcome: %w", err)
		}
		o.Outcome = &outcome
	}
	if len(r.recording) > 0 {
		var recording domain.RecordingFlags
		if err := json.Unmarshal(r.recording, &recording); err != nil {
			return domain.Order{}, fmt.Errorf("decode recording: %w", err)
		}
		o.Recording = &recording
	}
	if len(r.history) > 0 {
		var history []domain.AttemptRecord
		if err := json.Unmarshal(r.history, &history); err != nil {
			return domain.Order{}, fmt.Errorf("decode attempt history: %w", err)
		}
		o.AttemptHistory = history
	}
	return o, nil
}

func scanOrder(row rowScanner) (domain.Order, error) {
	var r orderRow
	if err := row.Scan(
		&r.id, &r.status, &r.clinic, &r.patient, &r.outcome, &r.recording,
		&r.createdAt, &r.updatedAt, &r.attempts, &r.nextAttemptAt, &r.history,
		&r.userID, &r.paidAt, &r.paymentReference,
	); err != nil {
		return domain.Order{}, err
	}
	return r.order()
}

// scanOrderWithLease additionally scans lease_until, used by Claim's locked re-check.
func scanOrderWithLease(row rowScanner) (domain.Order, *time.Time, error) {
	var r orderRow
	var leaseUntil *time.Time
	if err := row.Scan(
		&r.id, &r.status, &r.clinic, &r.patient, &r.outcome, &r.recording,
		&r.createdAt, &r.updatedAt, &r.attempts, &r.nextAttemptAt, &r.history,
		&r.userID, &r.paidAt, &r.paymentReference, &leaseUntil,
	); err != nil {
		return domain.Order{}, nil, err
	}
	order, err := r.order()
	return order, leaseUntil, err
}

// encodedOrder is the JSONB column payload for one order.
type encodedOrder struct {
	clinic           []byte
	patient          []byte
	outcome          []byte
	recording        []byte
	history          []byte
	userID           *string
	paidAt           *time.Time
	paymentReference *string
}

func encodeOrder(o domain.Order) (encodedOrder, error) {
	clinic, err := json.Marshal(o.Clinic)
	if err != nil {
		return encodedOrder{}, fmt.Errorf("encode clinic: %w", err)
	}
	patient, err := json.Marshal(o.Patient)
	if err != nil {
		return encodedOrder{}, fmt.Errorf("encode patient: %w", err)
	}
	var outcome []byte
	if o.Outcome != nil {
		outcome, err = json.Marshal(o.Outcome)
		if err != nil {
			return encodedOrder{}, fmt.Errorf("encode outcome: %w", err)
		}
	}
	var recording []byte
	if o.Recording != nil {
		recording, err = json.Marshal(o.Recording)
		if err != nil {
			return encodedOrder{}, fmt.Errorf("encode recording: %w", err)
		}
	}
	history := []byte("[]")
	if len(o.AttemptHistory) > 0 {
		history, err = json.Marshal(o.AttemptHistory)
		if err != nil {
			return encodedOrder{}, fmt.Errorf("encode attempt history: %w", err)
		}
	}
	enc := encodedOrder{
		clinic:           clinic,
		patient:          patient,
		outcome:          outcome,
		recording:        recording,
		history:          history,
		paidAt:           o.PaidAt,
		paymentReference: o.PaymentReference,
	}
	if o.UserID != "" {
		userID := string(o.UserID)
		enc.userID = &userID
	}
	return enc, nil
}
