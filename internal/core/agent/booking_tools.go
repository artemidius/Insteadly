package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// maxCaptureValueRunes bounds a captured value (1..2048 Unicode code points).
const maxCaptureValueRunes = 2048

var birthDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// bookingExecutor is the production ToolExecutor: it owns per-call proposal,
// confirmation, capture, and escalation state and persists only the allowed patient
// fields through the order store. One instance belongs to one agent run.
type bookingExecutor struct {
	orders      ports.OrderStore
	orderID     domain.OrderID
	now         func() time.Time
	preferences []domain.TimeRange

	proposal     *proposedSlot
	confirmation *confirmedSlot

	notes     []string
	escalated bool
	urgent    bool
}

type proposedSlot struct {
	start    time.Time
	end      time.Time
	doctor   string
	practice string
}

type confirmedSlot struct {
	proposedSlot
	code string
}

var _ ToolExecutor = (*bookingExecutor)(nil)

func newBookingExecutor(order domain.Order, orders ports.OrderStore, now func() time.Time) *bookingExecutor {
	prefs := append([]domain.TimeRange(nil), order.Patient.PreferredSlots...)
	return &bookingExecutor{orders: orders, orderID: order.ID, now: now, preferences: prefs}
}

// Execute dispatches one tool call. Unknown tools are a model-visible error, never an
// operational failure.
func (e *bookingExecutor) Execute(ctx context.Context, call ports.ToolCall) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch call.Name {
	case ToolProposeSlot:
		return e.proposeSlot(call.Arguments)
	case ToolConfirmBooking:
		return e.confirmBooking(call.Arguments)
	case ToolCaptureInfo:
		return e.captureInfo(ctx, call.Arguments)
	case ToolEscalateToHuman:
		return e.escalate(call.Arguments), nil
	default:
		return errorResult(errCodeUnknownTool), nil
	}
}

// --- propose_slot ---------------------------------------------------------------

type proposeArgs struct {
	Start    *string         `json:"start"`
	End      *string         `json:"end"`
	Doctor   json.RawMessage `json:"doctor"`
	Practice json.RawMessage `json:"practice"`
}

func (e *bookingExecutor) proposeSlot(raw []byte) ([]byte, error) {
	var args proposeArgs
	if res := decodeToolArguments(raw, &args); res != nil {
		return res, nil
	}
	if args.Start == nil || args.End == nil {
		return errorResult(errCodeInvalidArguments), nil
	}
	doctor, ok := decodeOptionalString(args.Doctor)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	practice, ok := decodeOptionalString(args.Practice)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	start, end, ok := parseInterval(*args.Start, *args.End)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	if !e.slotAllowed(start, end) {
		return errorResult(errCodeOutsidePrefs), nil
	}
	e.proposal = &proposedSlot{
		start:    start,
		end:      end,
		doctor:   strings.TrimSpace(doctor),
		practice: strings.TrimSpace(practice),
	}
	return marshalResult(proposeResult{OK: true, Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339)}), nil
}

// --- confirm_booking ------------------------------------------------------------

type confirmArgs struct {
	Start            *string         `json:"start"`
	End              *string         `json:"end"`
	Doctor           json.RawMessage `json:"doctor"`
	Practice         json.RawMessage `json:"practice"`
	ConfirmationCode json.RawMessage `json:"confirmation_code"`
}

func (e *bookingExecutor) confirmBooking(raw []byte) ([]byte, error) {
	var args confirmArgs
	if res := decodeToolArguments(raw, &args); res != nil {
		return res, nil
	}
	if args.Start == nil || args.End == nil {
		return errorResult(errCodeInvalidArguments), nil
	}
	doctor, ok := decodeOptionalString(args.Doctor)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	practice, ok := decodeOptionalString(args.Practice)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	code, ok := decodeOptionalString(args.ConfirmationCode)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	start, end, ok := parseInterval(*args.Start, *args.End)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	// A confirmation is only valid for the still-current successful proposal.
	if e.proposal == nil || !start.Equal(e.proposal.start) || !end.Equal(e.proposal.end) {
		return errorResult(errCodeNotConfirmed), nil
	}
	if !e.slotAllowed(start, end) {
		return errorResult(errCodeNotConfirmed), nil
	}
	// Identical repeated confirmations are locally idempotent.
	if e.confirmation != nil && e.confirmation.start.Equal(start) && e.confirmation.end.Equal(end) {
		return marshalResult(confirmResult{OK: true, ConfirmationCode: e.confirmation.code}), nil
	}
	e.confirmation = &confirmedSlot{
		proposedSlot: proposedSlot{
			start:    start,
			end:      end,
			doctor:   firstNonBlank(strings.TrimSpace(doctor), e.proposal.doctor),
			practice: firstNonBlank(strings.TrimSpace(practice), e.proposal.practice),
		},
		code: strings.TrimSpace(code),
	}
	return marshalResult(confirmResult{OK: true, ConfirmationCode: e.confirmation.code}), nil
}

// --- capture_info ---------------------------------------------------------------

type captureArgs struct {
	Field *string         `json:"field"`
	Value json.RawMessage `json:"value"`
}

func (e *bookingExecutor) captureInfo(ctx context.Context, raw []byte) ([]byte, error) {
	var args captureArgs
	if res := decodeToolArguments(raw, &args); res != nil {
		return res, nil
	}
	if args.Field == nil || len(args.Value) == 0 || isJSONNull(args.Value) {
		return errorResult(errCodeInvalidArguments), nil
	}
	// encoding/json replaces invalid UTF-8 with U+FFFD, so reject it on the raw token
	// before decoding rather than silently persisting a mutated value.
	if !utf8.Valid(args.Value) {
		return errorResult(errCodeInvalidValue), nil
	}
	decodedValue, ok := decodeRequiredRawString(args.Value)
	if !ok {
		return errorResult(errCodeInvalidArguments), nil
	}
	field := *args.Field
	value := strings.TrimSpace(decodedValue)
	if !allowedCaptureField(field) || !validCaptureValue(field, value) {
		return errorResult(errCodeInvalidValue), nil
	}

	if field == "note" {
		// The C0 domain has no note field. The value stays in per-call conversational
		// state only (it is already in the dialogue history) and is discarded at the end
		// of the call; it is never written to Outcome.Detail or a log.
		e.notes = append(e.notes, value)
		return marshalResult(captureResult{OK: true, Field: field}), nil
	}

	if _, err := e.orders.Update(ctx, e.orderID, func(o *domain.Order) error {
		switch field {
		case "full_name":
			o.Patient.FullName = value
		case "phone":
			o.Patient.Phone = value
		case "birth_date":
			o.Patient.BirthDate = value
		case "insurance_number":
			o.Patient.InsuranceNumber = value
		case "reason":
			o.Patient.Reason = value
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("capture_info: %w", err)
	}
	return marshalResult(captureResult{OK: true, Field: field}), nil
}

func allowedCaptureField(field string) bool {
	switch field {
	case "full_name", "phone", "birth_date", "insurance_number", "reason", "note":
		return true
	default:
		return false
	}
}

func validCaptureValue(field, value string) bool {
	if value == "" || utf8.RuneCountInString(value) > maxCaptureValueRunes {
		return false
	}
	if field == "birth_date" {
		if !birthDatePattern.MatchString(value) {
			return false
		}
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return false
		}
	}
	return true
}

// --- escalate_to_human ----------------------------------------------------------

type escalateArgs struct {
	Reason *string         `json:"reason"`
	Urgent json.RawMessage `json:"urgent"`
}

func (e *bookingExecutor) escalate(raw []byte) []byte {
	var args escalateArgs
	if res := decodeToolArguments(raw, &args); res != nil {
		return res
	}
	urgent, ok := decodeOptionalBool(args.Urgent)
	if !ok {
		return errorResult(errCodeInvalidArguments)
	}
	if args.Reason == nil || strings.TrimSpace(*args.Reason) == "" {
		return errorResult(errCodeInvalidArguments)
	}
	// Record only the fact/urgency; never the free-text reason.
	e.escalated = true
	e.urgent = urgent
	// No transfer target or telephony transfer port exists in C3, so the only truthful
	// validation result is unavailable. The model apologizes and finishes normally.
	return errorResult(errCodeUnavailable)
}

// --- outcome consistency --------------------------------------------------------

// ValidateOutcome enforces that a booked outcome is backed by a matching successful
// confirmation, and canonicalizes the booked slot from tool state.
func (e *bookingExecutor) ValidateOutcome(out *domain.Outcome) error {
	if out == nil {
		return fmt.Errorf("%w: nil outcome", domain.ErrInvalidOutcome)
	}
	if out.Kind != domain.OutcomeBooked {
		return nil
	}
	if e.confirmation == nil {
		return fmt.Errorf("%w: booked outcome without confirmed booking", domain.ErrInvalidOutcome)
	}
	if out.BookedSlot == nil || out.BookedSlot.DateTime.IsZero() {
		return fmt.Errorf("%w: booked outcome requires booked_slot", domain.ErrInvalidOutcome)
	}
	if !out.BookedSlot.DateTime.Equal(e.confirmation.start) {
		return fmt.Errorf("%w: booked slot does not match confirmation", domain.ErrInvalidOutcome)
	}
	doctor := strings.TrimSpace(out.BookedSlot.Doctor)
	if doctor != "" && doctor != e.confirmation.doctor {
		return fmt.Errorf("%w: booked doctor does not match confirmation", domain.ErrInvalidOutcome)
	}
	practice := strings.TrimSpace(out.BookedSlot.Practice)
	if practice != "" && practice != e.confirmation.practice {
		return fmt.Errorf("%w: booked practice does not match confirmation", domain.ErrInvalidOutcome)
	}
	out.BookedSlot.DateTime = e.confirmation.start
	out.BookedSlot.Doctor = firstNonBlank(doctor, e.confirmation.doctor)
	out.BookedSlot.Practice = firstNonBlank(practice, e.confirmation.practice)
	return nil
}

// decodeToolArguments turns strict-decode failures into the stable model-visible
// invalid_arguments result. A nil return means decoding succeeded.
func decodeToolArguments(raw []byte, v any) []byte {
	if err := decodeStrictObject(raw, v); err != nil {
		return errorResult(errCodeInvalidArguments)
	}
	return nil
}

// --- helpers --------------------------------------------------------------------

// parseInterval parses two RFC 3339 timestamps and requires start < end.
func parseInterval(rawStart, rawEnd string) (time.Time, time.Time, bool) {
	start, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rawStart))
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	end, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rawEnd))
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// slotAllowed applies the M0 policy: future start and full containment in one preferred
// range when preferences exist. No overlap heuristics, timezones, or clinic hours.
func (e *bookingExecutor) slotAllowed(start, end time.Time) bool {
	if !start.After(e.now()) {
		return false
	}
	if len(e.orderPreferences()) == 0 {
		return true
	}
	for _, pref := range e.orderPreferences() {
		if !start.Before(pref.Start) && !end.After(pref.End) {
			return true
		}
	}
	return false
}

// orderPreferences returns the preference snapshot taken at run start.
func (e *bookingExecutor) orderPreferences() []domain.TimeRange {
	return e.preferences
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
