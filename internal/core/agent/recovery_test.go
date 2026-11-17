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

func startRecovery(t *testing.T, cfg Config) (*conversation, *fakeSTT) {
	t.Helper()
	tts := &fakeTTS{}
	cfg.TTS = tts
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{
		{finals: []string{"frage eins"}},
		{finals: []string{"frage zwei"}},
		{finals: []string{"frage drei"}},
		{finals: []string{"frage vier"}},
	}}
	stt := cfg.STT.(*fakeSTT)
	c := startConversation(t, cfg)
	return c, stt
}

func TestRecoveryInvalidOutcomeThenValid(t *testing.T) {
	store := newFakeStore(testOrder())
	good := domain.Outcome{Kind: domain.OutcomeAborted, Detail: "abgebrochen"}
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Einen Moment."}, waitErr: domain.ErrInvalidOutcome},
		{chunks: []string{"Entschuldigung."}, result: ports.TurnResult{Outcome: &good}},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("outcome = %+v", outcome)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 1 {
		t.Fatalf("controls = %q, want 1", dlg.controls)
	}
	if !strings.Contains(dlg.controls[0], "finish_call") || !strings.Contains(dlg.controls[0], "schema") {
		t.Errorf("invalid-outcome control = %q", dlg.controls[0])
	}
	if len(dlg.interrupts) != 1 {
		t.Errorf("rollback interrupts = %d, want 1", len(dlg.interrupts))
	}
	if strings.Contains(dlg.controls[0], "frage eins") || strings.Contains(dlg.controls[0], "invalid") {
		t.Errorf("control leaked transcript/validation text: %q", dlg.controls[0])
	}
}

func TestRecoveryBookedWithoutConfirmationUsesControl(t *testing.T) {
	store := newFakeStore(testOrder())
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	unconfirmed := domain.Outcome{Kind: domain.OutcomeBooked, Detail: "gebucht", BookedSlot: &domain.BookedSlot{DateTime: slot}}
	fixed := domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "other", Detail: "nicht bestätigt"}
	dlg := &fakeDialogue{scripts: []turnScript{
		{result: ports.TurnResult{Outcome: &unconfirmed}},
		{chunks: []string{"Das konnte ich nicht bestätigen."}, result: ports.TurnResult{Outcome: &fixed}},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != domain.OutcomeFailed {
		t.Fatalf("outcome = %+v", outcome)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 1 || !strings.Contains(dlg.controls[0], "Werkzeugen") {
		t.Errorf("controls = %q", dlg.controls)
	}
	c.call.mu.Lock()
	if len(c.call.marks) == 0 {
		t.Error("no assistant audio was played for the repair")
	}
	c.call.mu.Unlock()
}

func TestRecoveryEmptyTurnThenReply(t *testing.T) {
	store := newFakeStore(testOrder())
	dlg := &fakeDialogue{scripts: []turnScript{
		{},
		{chunks: []string{"Wie kann ich helfen?"}},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "recovery control", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.controls) == 1
	})
	if !strings.Contains(dlg.controls[0], "leer") {
		t.Errorf("empty-turn control = %q", dlg.controls[0])
	}
	close(c.ended)
	if _, err := resultOf(t, c.done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoveryStallUsesControlWithoutRepeatingTools(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `"}`
	good := domain.Outcome{Kind: domain.OutcomeCallbackNeeded, Detail: "Rückruf", CallbackAt: ptrTime(time.Date(2027, 1, 16, 10, 0, 0, 0, time.UTC))}
	dlg := &fakeDialogue{scripts: []turnScript{
		{result: ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c1", Name: ToolProposeSlot, Arguments: []byte(propose)}}}},
		{waitCtx: never, waitErr: context.DeadlineExceeded},
		{result: ports.TurnResult{Outcome: &good}},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	cfg.AssistantTurnTimeout = 30 * time.Millisecond
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != domain.OutcomeCallbackNeeded {
		t.Fatalf("outcome = %+v", outcome)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 1 || !strings.Contains(dlg.controls[0], "ausgeblieben") {
		t.Errorf("stall control = %q", dlg.controls)
	}
	if len(dlg.results) != 1 {
		t.Errorf("tool results submitted = %d, want exactly 1", len(dlg.results))
	}
	if len(dlg.results[0]) != 1 || dlg.results[0][0].ID != "c1" {
		t.Errorf("tool result = %+v", dlg.results)
	}
}

func TestRecoveryExhaustionIsALimit(t *testing.T) {
	store := newFakeStore(testOrder())
	dlg := &fakeDialogue{scripts: []turnScript{
		{waitErr: domain.ErrInvalidOutcome},
		{waitErr: domain.ErrInvalidOutcome},
		{waitErr: domain.ErrInvalidOutcome},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	_, err := resultOf(t, c.done)
	if !errors.Is(err, errOutcomeRecoveryExhausted) {
		t.Fatalf("err = %v, want errOutcomeRecoveryExhausted", err)
	}
	if !IsLimit(err) {
		t.Fatal("recovery exhaustion was not classified as a limit")
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 2 {
		t.Errorf("controls = %d, want the per-call maximum of 2", len(dlg.controls))
	}
}

func TestRecoveryBudgetIsPerCallNotPerUserTurn(t *testing.T) {
	store := newFakeStore(testOrder())
	dlg := &fakeDialogue{scripts: []turnScript{
		{waitErr: domain.ErrInvalidOutcome},
		{chunks: []string{"ok eins"}},
		{waitErr: domain.ErrInvalidOutcome},
		{waitErr: domain.ErrInvalidOutcome},
	}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "first turn recovered", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.controls) == 1 && len(dlg.texts) >= 1
	})
	c.call.push(utteranceAudio()...)
	_, err := resultOf(t, c.done)
	if !errors.Is(err, errOutcomeRecoveryExhausted) {
		t.Fatalf("err = %v, want exhaustion on the shared per-call budget", err)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 2 {
		t.Errorf("controls = %d, want 2 total across both user turns", len(dlg.controls))
	}
}

func TestRecoveryDoesNotRepairOperationalErrors(t *testing.T) {
	store := newFakeStore(testOrder())
	sentinel := errors.New("provider transport down")
	dlg := &fakeDialogue{scripts: []turnScript{{waitErr: sentinel}}}
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	c, _ := startRecovery(t, cfg)
	c.call.push(utteranceAudio()...)
	_, err := resultOf(t, c.done)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the operational error", err)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 0 {
		t.Errorf("operational error was repaired with %q", dlg.controls)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
