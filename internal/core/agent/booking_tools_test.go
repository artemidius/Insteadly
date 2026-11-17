package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func newTestExecutor(t *testing.T, order domain.Order, now *time.Time) (*bookingExecutor, *fakeStore) {
	t.Helper()
	store := newFakeStore(order)
	return newBookingExecutor(order, store, func() time.Time { return *now }), store
}

func execute(t *testing.T, exec ToolExecutor, name, args string) []byte {
	t.Helper()
	result, err := exec.Execute(context.Background(), ports.ToolCall{ID: "call_1", Name: name, Arguments: []byte(args)})
	if err != nil {
		t.Fatalf("Execute(%s): %v", name, err)
	}
	return result
}

const (
	slotStart = "2027-01-15T09:00:00+01:00"
	slotEnd   = "2027-01-15T09:30:00+01:00"
)

func proposeArgsJSON(start, end string) string {
	return `{"start":"` + start + `","end":"` + end + `"}`
}

func TestProposeSlotInvalidArguments(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	cases := map[string]string{
		"empty":             `{}`,
		"missing end":       `{"start":"` + slotStart + `"}`,
		"missing start":     `{"end":"` + slotEnd + `"}`,
		"wrong type":        `{"start":1,"end":"` + slotEnd + `"}`,
		"unknown field":     `{"start":"` + slotStart + `","end":"` + slotEnd + `","extra":1}`,
		"trailing json":     `{"start":"` + slotStart + `","end":"` + slotEnd + `"} {}`,
		"malformed json":    `not-json`,
		"bad start":         propagatePropose(`{"start":"2027-01-15","end":"` + slotEnd + `"}`),
		"bad end":           propagatePropose(`{"start":"` + slotStart + `","end":"tomorrow"}`),
		"equal interval":    propagatePropose(proposeArgsJSON(slotStart, "2027-01-15T09:00:00+01:00")),
		"inverted interval": propagatePropose(proposeArgsJSON(slotEnd, slotStart)),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got := string(execute(t, exec, ToolProposeSlot, args))
			if got != `{"ok":false,"error":"invalid_arguments"}` {
				t.Errorf("result = %s", got)
			}
		})
	}
}

// propagatePropose keeps the table readable: most entries are literal argument payloads.
func propagatePropose(s string) string { return s }

func TestProposeSlotPreferencePolicy(t *testing.T) {
	order := testOrder()
	now := fixedNow
	exec, _ := newTestExecutor(t, order, &now)

	ok := string(execute(t, exec, ToolProposeSlot, proposeArgsJSON(slotStart, slotEnd)))
	if ok != `{"ok":true,"start":"2027-01-15T09:00:00+01:00","end":"2027-01-15T09:30:00+01:00"}` {
		t.Errorf("success result = %s", ok)
	}

	outside := []string{
		proposeArgsJSON("2027-01-15T07:00:00+01:00", "2027-01-15T07:30:00+01:00"),
		proposeArgsJSON("2027-01-15T11:45:00+01:00", "2027-01-15T12:15:00+01:00"),
	}
	for _, args := range outside {
		got := string(execute(t, exec, ToolProposeSlot, args))
		if got != `{"ok":false,"error":"outside_preferences"}` {
			t.Errorf("outside %s -> %s", args, got)
		}
	}

	// Overlap-only does not count as containment.
	got := string(execute(t, exec, ToolProposeSlot, proposeArgsJSON("2027-01-15T07:45:00+01:00", "2027-01-15T08:15:00+01:00")))
	if got != `{"ok":false,"error":"outside_preferences"}` {
		t.Errorf("overlap-only result = %s", got)
	}

	// The same instant expressed with another offset stays contained.
	crossOffset := string(execute(t, exec, ToolProposeSlot, proposeArgsJSON("2027-01-15T10:00:00+02:00", "2027-01-15T10:30:00+02:00")))
	if crossOffset != `{"ok":true,"start":"2027-01-15T10:00:00+02:00","end":"2027-01-15T10:30:00+02:00"}` {
		t.Errorf("cross-offset result = %s", crossOffset)
	}

	// A past start is outside preferences in v0.
	past := string(execute(t, exec, ToolProposeSlot, proposeArgsJSON("2026-11-17T18:00:00+01:00", "2026-11-17T18:30:00+01:00")))
	if past != `{"ok":false,"error":"outside_preferences"}` {
		t.Errorf("past result = %s", past)
	}
}

func TestProposeSlotEmptyPreferences(t *testing.T) {
	order := testOrder()
	order.Patient.PreferredSlots = nil
	now := fixedNow
	exec, _ := newTestExecutor(t, order, &now)
	got := string(execute(t, exec, ToolProposeSlot, proposeArgsJSON(slotStart, slotEnd)))
	if !strings.Contains(got, `"ok":true`) {
		t.Errorf("empty preferences should allow a future slot: %s", got)
	}
}

func TestConfirmBookingLifecycle(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)

	if got := string(execute(t, exec, ToolConfirmBooking, proposeArgsJSON(slotStart, slotEnd))); got != `{"ok":false,"error":"not_confirmed"}` {
		t.Errorf("confirm without proposal = %s", got)
	}

	execute(t, exec, ToolProposeSlot, proposeArgsJSON(slotStart, slotEnd))
	if got := string(execute(t, exec, ToolConfirmBooking, proposeArgsJSON("2027-01-15T10:00:00+01:00", "2027-01-15T10:30:00+01:00"))); got != `{"ok":false,"error":"not_confirmed"}` {
		t.Errorf("confirm different slot = %s", got)
	}

	// Another offset for the same instants is the same slot.
	confirm := `{"start":"2027-01-15T08:00:00Z","end":"2027-01-15T08:30:00Z","confirmation_code":"TEST-123"}`
	first := string(execute(t, exec, ToolConfirmBooking, confirm))
	if first != `{"ok":true,"confirmation_code":"TEST-123"}` {
		t.Fatalf("confirm result = %s", first)
	}
	second := string(execute(t, exec, ToolConfirmBooking, confirm))
	if second != first {
		t.Errorf("idempotent confirmation = %s, want %s", second, first)
	}

	// A blank code remains an empty string.
	exec2, _ := newTestExecutor(t, testOrder(), &now)
	execute(t, exec2, ToolProposeSlot, proposeArgsJSON(slotStart, slotEnd))
	if got := string(execute(t, exec2, ToolConfirmBooking, proposeArgsJSON(slotStart, slotEnd))); got != `{"ok":true,"confirmation_code":""}` {
		t.Errorf("empty code result = %s", got)
	}
}

func TestProposeReplacementAndConfirm(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	a := proposeArgsJSON(slotStart, slotEnd)
	b := proposeArgsJSON("2027-01-15T10:00:00+01:00", "2027-01-15T10:30:00+01:00")
	execute(t, exec, ToolProposeSlot, a)
	execute(t, exec, ToolProposeSlot, b)
	if got := string(execute(t, exec, ToolConfirmBooking, a)); got != `{"ok":false,"error":"not_confirmed"}` {
		t.Errorf("stale confirmation = %s", got)
	}
	if got := string(execute(t, exec, ToolConfirmBooking, b)); !strings.Contains(got, `"ok":true`) {
		t.Errorf("current confirmation = %s", got)
	}
}

func TestConfirmRevalidatesFuture(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	execute(t, exec, ToolProposeSlot, proposeArgsJSON(slotStart, slotEnd))
	now = time.Date(2027, 1, 15, 10, 0, 0, 0, time.UTC) // slot is now past
	if got := string(execute(t, exec, ToolConfirmBooking, proposeArgsJSON(slotStart, slotEnd))); got != `{"ok":false,"error":"not_confirmed"}` {
		t.Errorf("past confirmation = %s", got)
	}
}

func TestConfirmInvalidArguments(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	for _, args := range []string{
		`{}`,
		`{"start":"` + slotStart + `"}`,
		`{"start":"` + slotStart + `","end":"` + slotEnd + `","extra":1}`,
		`{"start":"nope","end":"` + slotEnd + `"}`,
		proposeArgsJSON(slotEnd, slotStart),
	} {
		if got := string(execute(t, exec, ToolConfirmBooking, args)); got != `{"ok":false,"error":"invalid_arguments"}` {
			t.Errorf("args %s -> %s", args, got)
		}
	}
}

func TestValidateOutcomeNonBookedPassthrough(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	callback := fixedNow.Add(time.Hour)
	for _, out := range []domain.Outcome{
		{Kind: domain.OutcomeCallbackNeeded, Detail: "callback", CallbackAt: &callback},
		{Kind: domain.OutcomeFailed, Detail: "failed", FailReason: "busy"},
		{Kind: domain.OutcomeAborted, Detail: "aborted"},
	} {
		copyOut := out
		if err := exec.ValidateOutcome(&copyOut); err != nil {
			t.Errorf("%s passthrough: %v", out.Kind, err)
		}
	}
}

func TestValidateOutcomeBookedRequiresConfirmation(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	out := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot},
	}
	if err := exec.ValidateOutcome(&out); !errors.Is(err, domain.ErrInvalidOutcome) {
		t.Fatalf("booked without confirmation err = %v, want ErrInvalidOutcome", err)
	}
}

func TestValidateOutcomeBookedCanonicalizes(t *testing.T) {
	now := fixedNow
	order := testOrder()
	exec, _ := newTestExecutor(t, order, &now)
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A"}`
	execute(t, exec, ToolProposeSlot, propose)
	execute(t, exec, ToolConfirmBooking, propose)
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))

	out := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot},
	}
	if err := exec.ValidateOutcome(&out); err != nil {
		t.Fatalf("ValidateOutcome: %v", err)
	}
	if out.BookedSlot.Doctor != "Dr. A" || out.BookedSlot.Practice != "Praxis A" {
		t.Errorf("blank doctor/practice not filled: %+v", out.BookedSlot)
	}

	// Matching values are preserved.
	withMatch := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot, Doctor: "Dr. A", Practice: "Praxis A"},
	}
	if err := exec.ValidateOutcome(&withMatch); err != nil {
		t.Fatalf("matching doctor/practice: %v", err)
	}

	// A model value that disagrees is rejected.
	mismatch := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot, Doctor: "Dr. B"},
	}
	if err := exec.ValidateOutcome(&mismatch); err == nil {
		t.Error("mismatched doctor accepted")
	}
	mismatchPractice := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot, Practice: "Praxis B"},
	}
	if err := exec.ValidateOutcome(&mismatchPractice); err == nil {
		t.Error("mismatched practice accepted")
	}

	// Wrong instant, nil slot, and nil outcome fail.
	wrongTime := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "booked",
		BookedSlot: &domain.BookedSlot{DateTime: slot.Add(time.Hour)},
	}
	if err := exec.ValidateOutcome(&wrongTime); err == nil {
		t.Error("wrong booked instant accepted")
	}
	nilSlot := domain.Outcome{Kind: domain.OutcomeBooked, Detail: "booked"}
	if err := exec.ValidateOutcome(&nilSlot); err == nil {
		t.Error("booked without slot accepted")
	}
	if err := exec.ValidateOutcome(nil); err == nil {
		t.Error("nil outcome accepted")
	}
}

func TestUnknownToolIsModelVisible(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	if got := string(execute(t, exec, ToolFinishCall, `{}`)); got != `{"ok":false,"error":"unknown_tool"}` {
		t.Errorf("finish_call dispatch = %s", got)
	}
	if got := string(execute(t, exec, "nope", `{}`)); got != `{"ok":false,"error":"unknown_tool"}` {
		t.Errorf("unknown tool dispatch = %s", got)
	}
}

func TestExecuteHonorsContext(t *testing.T) {
	now := fixedNow
	exec, _ := newTestExecutor(t, testOrder(), &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exec.Execute(ctx, ports.ToolCall{Name: ToolProposeSlot}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
