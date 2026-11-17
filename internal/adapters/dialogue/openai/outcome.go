package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// decodeFinishCall strictly decodes finish_call arguments into a domain.Outcome and
// validates it with §3.8 semantics plus the domain state machine.
func decodeFinishCall(args []byte) (domain.Outcome, error) {
	var out domain.Outcome
	if err := decodeStrictJSON(args, &out); err != nil {
		return domain.Outcome{}, fmt.Errorf("%w: %w", domain.ErrInvalidOutcome, err)
	}
	if err := validateOutcome(out); err != nil {
		return domain.Outcome{}, err
	}
	return out, nil
}

// decodeStrictJSON decodes exactly one JSON value, rejecting unknown fields and trailing
// data.
func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// validateOutcome applies the §3.8 conditional rules the domain does not (blank detail,
// zero time, irrelevant fields), then reuses the domain state machine for the rest.
func validateOutcome(out domain.Outcome) error {
	if strings.TrimSpace(out.Detail) == "" {
		return fmt.Errorf("%w: detail is required", domain.ErrInvalidOutcome)
	}
	switch out.Kind {
	case domain.OutcomeBooked:
		if out.CallbackAt != nil || out.FailReason != "" {
			return fmt.Errorf("%w: booked must not set callback_at or fail_reason", domain.ErrInvalidOutcome)
		}
	case domain.OutcomeCallbackNeeded:
		if out.CallbackAt == nil || out.CallbackAt.IsZero() {
			return fmt.Errorf("%w: callback_needed requires a valid callback_at", domain.ErrInvalidOutcome)
		}
		if out.BookedSlot != nil || out.FailReason != "" {
			return fmt.Errorf("%w: callback_needed must not set booked_slot or fail_reason", domain.ErrInvalidOutcome)
		}
	case domain.OutcomeFailed:
		if strings.TrimSpace(out.FailReason) == "" {
			return fmt.Errorf("%w: failed requires fail_reason", domain.ErrInvalidOutcome)
		}
		if out.BookedSlot != nil || out.CallbackAt != nil {
			return fmt.Errorf("%w: failed must not set booked_slot or callback_at", domain.ErrInvalidOutcome)
		}
	case domain.OutcomeAborted:
		if out.BookedSlot != nil || out.CallbackAt != nil || out.FailReason != "" {
			return fmt.Errorf("%w: aborted must not set booked_slot, callback_at, or fail_reason", domain.ErrInvalidOutcome)
		}
	}

	// Reuse domain validation (booked_slot, callback_at, fail_reason, known kind) without
	// touching a real order.
	o := domain.Order{Status: domain.OrderInCall}
	if err := o.Finish(out); err != nil {
		return err
	}
	return nil
}
