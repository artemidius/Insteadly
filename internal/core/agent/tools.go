package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// ToolExecutor executes the ordinary booking tools for one agent run.
//
// Validation failures are returned as a tool-specific JSON object; operational errors
// (storage/context) return error, which aborts the attempt rather than disguising the
// failure as model-visible invalid input.
type ToolExecutor interface {
	Execute(ctx context.Context, call ports.ToolCall) ([]byte, error)

	// ValidateOutcome checks tool/outcome consistency. For a booked outcome it requires
	// a matching successful confirmation and canonicalizes doctor/practice from that
	// confirmation. It never mutates or persists an Order.
	ValidateOutcome(out *domain.Outcome) error
}

// ToolFactory creates one executor per agent run (per call).
type ToolFactory func(order domain.Order) ToolExecutor

// Tool schemas, verbatim from the C3 plan §3.7. Every schema is a fresh copy per call.
var (
	proposeSlotSchema    = []byte(`{"type":"object","additionalProperties":false,"properties":{"start":{"type":"string","format":"date-time"},"end":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["start","end"]}`)
	confirmBookingSchema = []byte(`{"type":"object","additionalProperties":false,"properties":{"start":{"type":"string","format":"date-time"},"end":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"},"confirmation_code":{"type":"string"}},"required":["start","end"]}`)
	captureInfoSchema    = []byte(`{"type":"object","additionalProperties":false,"properties":{"field":{"type":"string","enum":["full_name","phone","birth_date","insurance_number","reason","note"]},"value":{"type":"string","minLength":1,"maxLength":2048}},"required":["field","value"]}`)
	escalateSchema       = []byte(`{"type":"object","additionalProperties":false,"properties":{"reason":{"type":"string","minLength":1},"urgent":{"type":"boolean"}},"required":["reason"]}`)
)

// ToolDefinitions returns exactly the four ordinary tools, with fresh schema slices so a
// caller or adapter cannot mutate another call's definitions.
func ToolDefinitions() []domain.Tool {
	return []domain.Tool{
		{
			Name:        ToolProposeSlot,
			Description: "Propose one appointment slot offered by the clinic receptionist. This records a proposal only and never books. start/end are RFC 3339 timestamps and start must be before end. If the slot is outside the patient's preferred slots, the result is outside_preferences; ask for another slot.",
			Parameters:  append([]byte(nil), proposeSlotSchema...),
		},
		{
			Name:        ToolConfirmBooking,
			Description: "Record that the receptionist explicitly accepted the exact slot that was successfully proposed first. Call this only after explicit acceptance; it does not finish the call.",
			Parameters:  append([]byte(nil), confirmBookingSchema...),
		},
		{
			Name:        ToolCaptureInfo,
			Description: "Persist one piece of patient information that the receptionist explicitly supplied and is authorized to provide. field is one of full_name, phone, birth_date, insurance_number, reason, note. Never invent values.",
			Parameters:  append([]byte(nil), captureInfoSchema...),
		},
		{
			Name:        ToolEscalateToHuman,
			Description: "Request a handoff to a human when one is genuinely required. A handoff may be unavailable in this version; report that honestly instead of claiming a transfer happened.",
			Parameters:  append([]byte(nil), escalateSchema...),
		},
	}
}

// Tool error codes are stable strings the model can react to.
const (
	errCodeUnknownTool      = "unknown_tool"
	errCodeInvalidArguments = "invalid_arguments"
	errCodeInvalidValue     = "invalid_value"
	errCodeOutsidePrefs     = "outside_preferences"
	errCodeNotConfirmed     = "not_confirmed"
	errCodeUnavailable      = "unavailable"
)

// result envelopes: always include ok; success shapes include every specified field.
type toolErrorResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type proposeResult struct {
	OK    bool   `json:"ok"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type confirmResult struct {
	OK               bool   `json:"ok"`
	ConfirmationCode string `json:"confirmation_code"`
}

type captureResult struct {
	OK    bool   `json:"ok"`
	Field string `json:"field"`
}

func marshalResult(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// The result types above always marshal; keep a stable error shape if not.
		return []byte(`{"ok":false,"error":"invalid_arguments"}`)
	}
	return b
}

func errorResult(code string) []byte {
	return marshalResult(toolErrorResult{OK: false, Error: code})
}

// decodeStrictObject decodes exactly one JSON object, rejecting unknown fields and
// trailing values. All model output is untrusted even though C2 validates broad shape.
func decodeStrictObject(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

// isJSONNull reports whether an optional raw JSON field is explicitly null.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// decodeOptionalString decodes an optional JSON string, rejecting null and wrong types.
// A missing field is a valid empty value; every present field must be a real string.
func decodeOptionalString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if isJSONNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// decodeRequiredRawString decodes a present JSON string token, rejecting null and wrong
// types. Invalid UTF-8 is checked by the caller on the raw token before decoding.
func decodeRequiredRawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// decodeOptionalBool decodes an optional JSON boolean, rejecting null and wrong types.
func decodeOptionalBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, true
	}
	if isJSONNull(raw) {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

// jsonValidObject reports whether b is exactly one JSON object.
func jsonValidObject(b []byte) bool {
	if len(b) == 0 || b[0] != '{' || !json.Valid(b) {
		return false
	}
	return true
}
