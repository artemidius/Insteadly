package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestIvrParserGermanAndEnglishMenus(t *testing.T) {
	cases := []struct {
		text       string
		wantMenu   bool
		wantTarget []string
	}{
		{"Drücken Sie die 1 für Termine.", true, []string{"1"}},
		{"Für einen Termin drücken Sie 1.", true, []string{"1"}},
		{"Press 1 for appointments.", true, []string{"1"}},
		{"For reception press 0.", true, []string{"0"}},
		{"Drücken Sie eins für Termine.", true, []string{"1"}},
		{"Drücken Sie Sternchen für die Wiederholung.", true, nil},
		{"Drücken Sie Raute für weitere Optionen.", true, nil},
		{"Drücken Sie 1 für Termine oder 2 für die Terminvergabe.", true, []string{"1", "2"}},
		{"Drücken Sie 1 für Rezepte.", true, nil},
		{"Drücken Sie 1 bei Notfall.", true, nil},
		{"Am 15. Jänner haben wir Termine.", false, nil},
		{"Rufen Sie 0664 123456 an.", false, nil},
		{"Wir sind von 8 bis 12 Uhr geöffnet.", false, nil},
		{"Ich möchte einen Termin am 1. Februar.", false, nil},
		{"Guten Tag, ich habe eine Frage.", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			p := parseMenu(tc.text)
			if p.IsMenu != tc.wantMenu {
				t.Fatalf("IsMenu = %v, want %v (options=%+v)", p.IsMenu, tc.wantMenu, p.Options)
			}
			if len(p.Targets) != len(tc.wantTarget) {
				t.Fatalf("targets = %v, want %v", p.Targets, tc.wantTarget)
			}
			for i := range tc.wantTarget {
				if p.Targets[i] != tc.wantTarget[i] {
					t.Errorf("target %d = %q, want %q", i, p.Targets[i], tc.wantTarget[i])
				}
			}
			if p.Ambiguous != (len(p.Targets) > 1) {
				t.Errorf("ambiguous = %v", p.Ambiguous)
			}
		})
	}
}

func ivrConversation(t *testing.T, finals []string, dlg *fakeDialogue) (*conversation, *fakeSTT) {
	t.Helper()
	scripts := make([]fakeSTTScript, len(finals))
	for i, f := range finals {
		scripts[i] = fakeSTTScript{finals: []string{f}}
	}
	stt := &fakeSTT{scripts: scripts}
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = stt
	if dlg == nil {
		dlg = textOnlyDialogue("Gerne.")
	}
	cfg.Dialogue = dlg
	c := startConversation(t, cfg)
	return c, stt
}

func TestIvrUniqueTargetSendsOneDigitThenHumanDialogue(t *testing.T) {
	dlg := textOnlyDialogue("Einen Moment.")
	c, _ := ivrConversation(t, []string{
		"Drücken Sie die 1 für Termine.",
		"Ich möchte einen Termin vereinbaren.",
	}, dlg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "one digit pressed", func() bool {
		c.call.mu.Lock()
		defer c.call.mu.Unlock()
		return len(c.call.dtmf) == 1
	})
	c.call.mu.Lock()
	digits := append([]string(nil), c.call.dtmf...)
	c.call.mu.Unlock()
	if digits[0] != "1" {
		t.Fatalf("digits = %q", digits)
	}
	dlg.mu.Lock()
	opens := dlg.opens
	dlg.mu.Unlock()
	if opens != 0 {
		t.Fatalf("menu transcript opened a dialogue session")
	}
	c.call.push(utteranceAudio()...)
	waitFor(t, "human turn reaches dialogue", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	close(c.ended)
	if _, err := resultOf(t, c.done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v", err)
	}
}

func TestIvrRepeatedMenuIsDeadEnd(t *testing.T) {
	dlg := textOnlyDialogue("x")
	c, _ := ivrConversation(t, []string{
		"Drücken Sie die 1 für Termine.",
		"Drücken Sie die 1 für Termine.",
	}, dlg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "first digit", func() bool {
		c.call.mu.Lock()
		defer c.call.mu.Unlock()
		return len(c.call.dtmf) == 1
	})
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("ivr dead end returned an operational error: %v", err)
	}
	if outcome.Kind != domain.OutcomeFailed || outcome.FailReason != "ivr_dead_end" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Detail != detailIVRDeadEnd {
		t.Errorf("detail = %q", outcome.Detail)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if dlg.opens != 0 {
		t.Errorf("IVR-only dead end created a dialogue session")
	}
}

func TestIvrAmbiguousOrNonTargetMenuTwiceIsDeadEnd(t *testing.T) {
	for name, menu := range map[string]string{
		"ambiguous": "Drücken Sie 1 für Termine oder 2 für die Terminvergabe.",
		"excluded":  "Drücken Sie 1 für Rezepte.",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := ivrConversation(t, []string{menu, menu}, textOnlyDialogue("x"))
			c.call.push(utteranceAudio()...)
			time.Sleep(100 * time.Millisecond)
			c.call.push(utteranceAudio()...)
			outcome, err := resultOf(t, c.done)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if outcome.FailReason != "ivr_dead_end" {
				t.Fatalf("outcome = %+v", outcome)
			}
			c.call.mu.Lock()
			defer c.call.mu.Unlock()
			if len(c.call.dtmf) != 0 {
				t.Errorf("a non-target/ambiguous menu sent digits: %q", c.call.dtmf)
			}
		})
	}
}

func TestIvrSendFailureIsOperationalNotDeadEnd(t *testing.T) {
	c, _ := ivrConversation(t, []string{"Drücken Sie 1 für Termine."}, textOnlyDialogue("x"))
	sentinel := errors.New("dtmf transport down")
	c.call.mu.Lock()
	c.call.dtmfErr = sentinel
	c.call.mu.Unlock()
	c.call.push(utteranceAudio()...)
	_, err := resultOf(t, c.done)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the DTMF send failure", err)
	}
	if strings.Contains(err.Error(), "ivr_dead_end") {
		t.Error("send failure was reported as an IVR dead end")
	}
}

func TestIvrWaitTimeoutIsDeadEnd(t *testing.T) {
	cfgSTT := []string{"Drücken Sie 1 für Termine."}
	scripts := []fakeSTTScript{{finals: []string{cfgSTT[0]}}}
	stt := &fakeSTT{scripts: scripts}
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = stt
	cfg.Dialogue = textOnlyDialogue("x")
	cfg.IVRWaitTimeout = 30 * time.Millisecond
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.FailReason != "ivr_dead_end" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestInboundDTMFIsEvidenceNotEcho(t *testing.T) {
	c, _ := ivrConversation(t, []string{"Ich hätte gern einen Termin."}, textOnlyDialogue("Gerne."))
	// Evidence about the far end arrives before the human turn.
	c.call.controls <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "7"}
	c.call.push(utteranceAudio()...)
	waitFor(t, "dialogue turn", func() bool {
		c.call.mu.Lock()
		defer c.call.mu.Unlock()
		return len(c.call.dtmf) == 0
	})
	close(c.ended)
	_, _ = resultOf(t, c.done)
	c.call.mu.Lock()
	defer c.call.mu.Unlock()
	if len(c.call.dtmf) != 0 {
		t.Errorf("inbound DTMF was echoed back: %q", c.call.dtmf)
	}
}

func TestIvrOfficeHoursMenuDoesNotPressAppointmentDigit(t *testing.T) {
	// The appointment phrase belongs to a different, digit-less option clause; it must not
	// supply the purpose for digit 1.
	menu := "Press 1 for office hours; for appointments, stay on the line."
	p := parseMenu(menu)
	if !p.IsMenu {
		t.Fatal("menu syntax not recognized")
	}
	if len(p.Targets) != 0 {
		t.Fatalf("targets = %v, want none (wrong-digit hazard)", p.Targets)
	}

	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{
		{finals: []string{menu}},
		{finals: []string{menu}},
	}}
	cfg.Dialogue = textOnlyDialogue("x")
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	time.Sleep(100 * time.Millisecond)
	c.call.push(utteranceAudio()...)
	outcome, err := resultOf(t, c.done)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.FailReason != "ivr_dead_end" {
		t.Fatalf("outcome = %+v", outcome)
	}
	c.call.mu.Lock()
	defer c.call.mu.Unlock()
	if len(c.call.dtmf) != 0 {
		t.Fatalf("office-hours menu pressed %q", c.call.dtmf)
	}
}

func TestIvrOptionLocalPurposeSelectsOnlyTheMatchingDigit(t *testing.T) {
	cases := map[string][]string{
		"Drücken Sie 1 für Termine; für Rezepte drücken Sie 2.":    {"1"},
		"Für Termine drücken Sie 1, für Rezepte drücken Sie 2.":    {"1"},
		"Drücken Sie 1 für Rezepte; für Termine bleiben Sie dran.": nil,
	}
	for text, want := range cases {
		p := parseMenu(text)
		if !p.IsMenu {
			t.Fatalf("%q: not recognized as a menu", text)
		}
		if len(p.Targets) != len(want) {
			t.Fatalf("%q: targets = %v, want %v", text, p.Targets, want)
		}
		for i := range want {
			if p.Targets[i] != want[i] {
				t.Errorf("%q: target %d = %q, want %q", text, i, p.Targets[i], want[i])
			}
		}
	}
}

func TestIvrHumanActivityCancelsPostDigitTimeout(t *testing.T) {
	cfg := testConfig(newFakeStore(testOrder()))
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{
		{finals: []string{"Drücken Sie die 1 für Termine."}},
		{finals: []string{"Ich habe noch eine Frage."}},
	}}
	dlg := textOnlyDialogue("Gerne.")
	cfg.Dialogue = dlg
	cfg.IVRWaitTimeout = 60 * time.Millisecond
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "digit sent", func() bool {
		c.call.mu.Lock()
		defer c.call.mu.Unlock()
		return len(c.call.dtmf) == 1
	})
	// A human final arrives well inside the post-digit window and enters dialogue.
	c.call.push(utteranceAudio()...)
	waitFor(t, "human turn answered", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	// Wait past the original IVR deadline: the stale timer must not fire a dead end.
	time.Sleep(150 * time.Millisecond)
	select {
	case r := <-c.done:
		t.Fatalf("stale IVR timeout fired after a human turn: outcome=%+v err=%v", r.outcome, r.err)
	default:
	}
	close(c.ended)
	if _, err := resultOf(t, c.done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v", err)
	}
}
