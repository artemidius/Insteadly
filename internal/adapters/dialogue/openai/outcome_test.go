package openai

import (
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestDecodeFinishCallBooked(t *testing.T) {
	args := []byte(`{"kind":"booked","detail":"Termin bestätigt","booked_slot":{"date_time":"2026-11-20T09:00:00Z","doctor":"Dr. Muster","practice":"Hietzing"}}`)
	out, err := decodeFinishCall(args)
	if err != nil {
		t.Fatalf("decodeFinishCall: %v", err)
	}
	if out.Kind != domain.OutcomeBooked || out.BookedSlot == nil || out.BookedSlot.Doctor != "Dr. Muster" {
		t.Errorf("outcome = %+v", out)
	}
	if out.CallbackAt != nil || out.FailReason != "" {
		t.Errorf("booked must not set callback/fail: %+v", out)
	}
}

func TestDecodeFinishCallCallbackNeeded(t *testing.T) {
	at := "2026-11-20T10:00:00Z"
	args := []byte(`{"kind":"callback_needed","detail":"Rückruf","callback_at":"` + at + `"}`)
	out, err := decodeFinishCall(args)
	if err != nil {
		t.Fatalf("decodeFinishCall: %v", err)
	}
	if out.Kind != domain.OutcomeCallbackNeeded || out.CallbackAt == nil || !out.CallbackAt.Equal(mustTime(at)) {
		t.Errorf("outcome = %+v", out)
	}
}

func TestDecodeFinishCallFailed(t *testing.T) {
	args := []byte(`{"kind":"failed","detail":"Kein Termin","fail_reason":"busy"}`)
	out, err := decodeFinishCall(args)
	if err != nil {
		t.Fatalf("decodeFinishCall: %v", err)
	}
	if out.Kind != domain.OutcomeFailed || out.FailReason != "busy" {
		t.Errorf("outcome = %+v", out)
	}
}

func TestDecodeFinishCallAborted(t *testing.T) {
	args := []byte(`{"kind":"aborted","detail":"Abgebrochen"}`)
	out, err := decodeFinishCall(args)
	if err != nil {
		t.Fatalf("decodeFinishCall: %v", err)
	}
	if out.Kind != domain.OutcomeAborted {
		t.Errorf("outcome = %+v", out)
	}
}

func TestDecodeFinishCallRejects(t *testing.T) {
	cases := map[string]string{
		"blank detail":          `{"kind":"aborted","detail":" "}`,
		"unknown kind":          `{"kind":"nope","detail":"d"}`,
		"unknown field":         `{"kind":"aborted","detail":"d","bogus":1}`,
		"booked missing slot":   `{"kind":"booked","detail":"d"}`,
		"booked extra callback": `{"kind":"booked","detail":"d","booked_slot":{"date_time":"2026-11-20T09:00:00Z"},"callback_at":"2026-11-20T09:00:00Z"}`,
		"booked extra fail":     `{"kind":"booked","detail":"d","booked_slot":{"date_time":"2026-11-20T09:00:00Z"},"fail_reason":"x"}`,
		"callback missing time": `{"kind":"callback_needed","detail":"d"}`,
		"callback zero time":    `{"kind":"callback_needed","detail":"d","callback_at":"0001-01-01T00:00:00Z"}`,
		"callback extra slot":   `{"kind":"callback_needed","detail":"d","callback_at":"2026-11-20T09:00:00Z","booked_slot":{"date_time":"2026-11-20T09:00:00Z"}}`,
		"failed missing reason": `{"kind":"failed","detail":"d"}`,
		"failed blank reason":   `{"kind":"failed","detail":"d","fail_reason":" "}`,
		"failed extra slot":     `{"kind":"failed","detail":"d","fail_reason":"busy","booked_slot":{"date_time":"2026-11-20T09:00:00Z"}}`,
		"aborted extra slot":    `{"kind":"aborted","detail":"d","booked_slot":{"date_time":"2026-11-20T09:00:00Z"}}`,
		"trailing json":         `{"kind":"aborted","detail":"d"} {}`,
		"not object":            `[]`,
		"malformed time":        `{"kind":"booked","detail":"d","booked_slot":{"date_time":"not-a-time"}}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeFinishCall([]byte(args))
			if err == nil {
				t.Fatalf("expected error for %s", args)
			}
			if !errors.Is(err, domain.ErrInvalidOutcome) {
				t.Errorf("err = %v, want domain.ErrInvalidOutcome wrap", err)
			}
		})
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
