package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
)

// This file exercises the full credential-free M1 conversation chain: real C2 Mistral
// STT/TTS/dialogue adapters against loopback fake servers, the real core conversation
// engine, and scripted fake telephony. It asserts provider-visible observations (streamed
// STT, clear/marks, DTMF, chat history), not only the final outcome.

func TestM1IntegrationBargeInOnlyConfirmedHistory(t *testing.T) {
	h := newHarnessWith(t, func(n int, _ chatRequest) chatScript {
		switch n {
		case 1:
			return chatScript{texts: []string{"Erster Satz. Zweiter Satz. Dritter Satz."}}
		case 2:
			return chatScript{
				texts: []string{"Verstanden."},
				calls: []wireCall{{id: "call_finish", name: "finish_call", args: `{"kind":"aborted","detail":"Testabbruch"}`}},
			}
		default:
			return chatScript{texts: []string{"unerwartet"}}
		}
	}, nil)
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{"Erste Frage", "Zweite Frage"}
	h.mistral.ttsDelay = map[string]time.Duration{"Dritter Satz.": 500 * time.Millisecond}
	h.mistral.mu.Unlock()

	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	// Greeting has two confirmed segments; the first two assistant segments add two more.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.session.mu.Lock()
		marks := h.session.marksAcked
		h.session.mu.Unlock()
		if marks >= 4 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}

	h.session.mu.Lock()
	clears := h.session.clearCount
	h.session.mu.Unlock()
	if clears != 1 {
		t.Errorf("clear count = %d, want exactly one barge clear", clears)
	}

	h.mistral.mu.Lock()
	history := append([]chatRequest(nil), h.mistral.chatHistory...)
	chats := len(history)
	sttStreams := h.mistral.sttStreams
	h.mistral.mu.Unlock()
	if chats != 2 {
		t.Fatalf("chat requests = %d, want 2", chats)
	}
	if sttStreams != 2 {
		t.Errorf("STT streams = %d, want one per utterance", sttStreams)
	}
	var assistantTexts, userTexts []string
	for _, m := range history[1].Messages {
		switch m.Role {
		case "assistant":
			assistantTexts = append(assistantTexts, m.Content)
		case "user":
			userTexts = append(userTexts, m.Content)
		}
	}
	joined := strings.Join(assistantTexts, " | ")
	if !strings.Contains(joined, "Erster Satz.") || !strings.Contains(joined, "Zweiter Satz.") {
		t.Errorf("confirmed assistant history missing: %q", joined)
	}
	if strings.Contains(joined, "Dritter Satz.") {
		t.Errorf("unmarked assistant segment survived interruption: %q", joined)
	}
	if got := strings.Join(userTexts, " | "); !strings.Contains(got, "Zweite Frage") {
		t.Errorf("barging user final missing from history: %q", got)
	}
}

func TestM1IntegrationIVRDigitThenBooked(t *testing.T) {
	h := newHarness(t, bookedScript())
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{
		"Drücken Sie die 1 für Termine.",
		"Ja, wir haben am 15. Jänner um neun Uhr einen Termin, der passt, ich bestätige das.",
	}
	h.mistral.mu.Unlock()

	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.session.mu.Lock()
		digits := append([]string(nil), h.session.dtmf...)
		h.session.mu.Unlock()
		if len(digits) > 0 {
			if len(digits) != 1 || digits[0] != "1" {
				t.Fatalf("digits = %q, want exactly one 1", digits)
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	history := append([]chatRequest(nil), h.mistral.chatHistory...)
	h.mistral.mu.Unlock()
	if len(history) != 3 {
		t.Fatalf("chat requests = %d, want 3 (menu is not dialogue)", len(history))
	}
	for _, m := range history[0].Messages {
		if strings.Contains(m.Content, "Drücken Sie") {
			t.Fatalf("menu transcript was sent to dialogue")
		}
	}
	h.session.mu.Lock()
	digits := append([]string(nil), h.session.dtmf...)
	h.session.mu.Unlock()
	if len(digits) != 1 || digits[0] != "1" {
		t.Errorf("digits = %q, want exactly one 1", digits)
	}
}

func TestM1IntegrationIVRAmbiguityIsDeadEnd(t *testing.T) {
	h := newHarness(t, func(int, chatRequest) chatScript { return chatScript{} })
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{
		"Drücken Sie 1 für Termine oder 2 für die Terminvergabe.",
		"Drücken Sie 1 für Termine oder 2 für die Terminvergabe.",
	}
	h.mistral.mu.Unlock()
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	time.Sleep(150 * time.Millisecond)
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("ivr dead end returned an operational error: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.FailReason != "ivr_dead_end" {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	chats := len(h.mistral.chatHistory)
	h.mistral.mu.Unlock()
	if chats != 0 {
		t.Errorf("ambiguity opened %d dialogue request(s)", chats)
	}
	h.session.mu.Lock()
	digits := len(h.session.dtmf)
	h.session.mu.Unlock()
	if digits != 0 {
		t.Errorf("ambiguous menu sent %d digit(s)", digits)
	}
}

func TestM1IntegrationHoldResumeWithoutSTT(t *testing.T) {
	h := newHarness(t, func(n int, _ chatRequest) chatScript {
		if n == 1 {
			return chatScript{
				texts: []string{"Gerne."},
				calls: []wireCall{{id: "call_finish", name: "finish_call", args: `{"kind":"callback_needed","detail":"Rückruf","callback_at":"2027-01-16T10:00:00+01:00"}`}},
			}
		}
		return chatScript{texts: []string{"unerwartet"}}
	})
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{"Ich möchte einen Termin."}
	h.mistral.mu.Unlock()
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	// 3 s of hold tone: no STT stream and no dialogue request may be created.
	h.session.sendFrames(holdTonePCM())
	time.Sleep(150 * time.Millisecond)
	h.mistral.mu.Lock()
	streams := h.mistral.sttStreams
	chats := len(h.mistral.chatHistory)
	h.mistral.mu.Unlock()
	if streams != 0 {
		t.Errorf("hold audio opened %d STT stream(s)", streams)
	}
	if chats != 0 {
		t.Errorf("hold audio created %d dialogue request(s)", chats)
	}
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.Kind != domain.OutcomeCallbackNeeded {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	streams = h.mistral.sttStreams
	h.mistral.mu.Unlock()
	if streams != 1 {
		t.Errorf("STT streams = %d, want only the resumed human utterance", streams)
	}
}

func TestM1IntegrationHoldTimeout(t *testing.T) {
	h := newHarnessWith(t, func(int, chatRequest) chatScript { return chatScript{} }, func(cfg *agent.Config) {
		cfg.Hold.MaxContinuous = 50 * time.Millisecond
	})
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(holdTonePCM())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("hold timeout returned an operational error: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.FailReason != "other" || r.final.Outcome.Detail != "hold timeout" {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	streams := h.mistral.sttStreams
	chats := len(h.mistral.chatHistory)
	h.mistral.mu.Unlock()
	if streams != 0 || chats != 0 {
		t.Errorf("hold timeout used STT/dialogue: streams=%d chats=%d", streams, chats)
	}
}

func TestM1IntegrationInvalidOutcomeRepair(t *testing.T) {
	h := newHarness(t, func(n int, _ chatRequest) chatScript {
		switch n {
		case 1:
			return chatScript{
				texts: []string{"Einen Moment."},
				calls: []wireCall{{id: "call_bad", name: "finish_call", args: `{"kind":"booked","detail":"ohne Slot"}`}},
			}
		case 2:
			return chatScript{
				texts: []string{"Entschuldigung."},
				calls: []wireCall{{id: "call_good", name: "finish_call", args: `{"kind":"aborted","detail":"Testabbruch"}`}},
			}
		default:
			return chatScript{texts: []string{"unerwartet"}}
		}
	})
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{"Buchen bitte."}
	h.mistral.mu.Unlock()
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	history := append([]chatRequest(nil), h.mistral.chatHistory...)
	h.mistral.mu.Unlock()
	if len(history) != 2 {
		t.Fatalf("chat requests = %d, want 2", len(history))
	}
	last := history[1].Messages[len(history[1].Messages)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "finish_call") {
		t.Errorf("repair control = %+v, want a system control message", last)
	}
	for _, m := range history[1].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "finish_call") {
			t.Error("repair instruction was represented as user speech")
		}
	}
	if r.final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", r.final.Attempts)
	}
}

func TestM1IntegrationBookedWithoutConfirmationRepaired(t *testing.T) {
	slot := "2027-01-15T09:00:00+01:00"
	h := newHarness(t, func(n int, _ chatRequest) chatScript {
		switch n {
		case 1:
			return chatScript{
				calls: []wireCall{{id: "call_bad", name: "finish_call", args: `{"kind":"booked","detail":"gebucht","booked_slot":{"date_time":"` + slot + `"}}`}},
			}
		case 2:
			return chatScript{
				calls: []wireCall{{id: "call_good", name: "finish_call", args: `{"kind":"failed","detail":"nicht bestätigt","fail_reason":"other"}`}},
			}
		default:
			return chatScript{texts: []string{"unerwartet"}}
		}
	})
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{"Bitte buchen."}
	h.mistral.mu.Unlock()
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if r.final.Outcome == nil || r.final.Outcome.FailReason != "other" {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	h.mistral.mu.Lock()
	history := append([]chatRequest(nil), h.mistral.chatHistory...)
	h.mistral.mu.Unlock()
	if len(history) != 2 {
		t.Fatalf("chat requests = %d, want 2", len(history))
	}
	last := history[1].Messages[len(history[1].Messages)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "Werkzeugen") {
		t.Errorf("consistency control = %+v", last)
	}
}

func TestM1IntegrationRecoveryExhaustion(t *testing.T) {
	h := newHarness(t, func(int, chatRequest) chatScript {
		return chatScript{
			calls: []wireCall{{id: "call_bad", name: "finish_call", args: `{"kind":"booked","detail":"ohne Slot"}`}},
		}
	})
	h.mistral.mu.Lock()
	h.mistral.sttFinals = []string{"Bitte buchen."}
	h.mistral.mu.Unlock()
	done := h.start(t)
	h.session.sendProgress()
	h.awaitGreeting(t)
	h.session.sendFrames(utteranceFrames())
	r := h.waitRun(t, done)
	if r.final.Outcome == nil || r.final.Outcome.Kind != domain.OutcomeFailed || r.final.Outcome.FailReason != "other" {
		t.Fatalf("outcome = %+v", r.final.Outcome)
	}
	if r.final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", r.final.Attempts)
	}
	h.mistral.mu.Lock()
	chats := len(h.mistral.chatHistory)
	h.mistral.mu.Unlock()
	if chats != 3 {
		t.Errorf("chat requests = %d, want initial + 2 recovery controls", chats)
	}
}

func holdTonePCM() [][]byte {
	out := make([][]byte, 0, 160)
	for i := 0; i < 160; i++ {
		out = append(out, tonePCMFrame(440, 12000))
	}
	return out
}
