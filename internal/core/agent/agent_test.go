package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runAgent(t *testing.T, a *Agent, order domain.Order, call *fakeCall, ended <-chan domain.CallEvent) (domain.Outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.Run(ctx, order, call, ended, call.Control())
}

// startAgent runs the agent in the background and returns a channel for its result.
func startAgent(t *testing.T, a *Agent, order domain.Order, call *fakeCall, ended <-chan domain.CallEvent) <-chan struct {
	outcome domain.Outcome
	err     error
} {
	t.Helper()
	done := make(chan struct {
		outcome domain.Outcome
		err     error
	}, 1)
	go func() {
		outcome, err := runAgent(t, a, order, call, ended)
		done <- struct {
			outcome domain.Outcome
			err     error
		}{outcome, err}
	}()
	return done
}

func resultOf(t *testing.T, done <-chan struct {
	outcome domain.Outcome
	err     error
}) (domain.Outcome, error) {
	t.Helper()
	select {
	case r := <-done:
		return r.outcome, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not return")
		return domain.Outcome{}, nil
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	root := testConfig(newFakeStore())
	a := fastAgent(t, root)
	if a.cfg.VAD.StartFrames != defaultStartFrames ||
		a.cfg.VAD.BargeInFrames != defaultBargeInFrames ||
		a.cfg.Hold.WindowFrames != defaultHoldWindowFrames ||
		a.cfg.ListenTimeout != defaultListenTimeout ||
		a.cfg.MaxUserTurns != defaultMaxUserTurns ||
		a.cfg.MaxToolRounds != defaultMaxToolRounds ||
		a.cfg.MaxCallDuration != defaultMaxCallDuration ||
		a.cfg.AssistantTurnTimeout != defaultAssistantTurnTimeout ||
		a.cfg.PlaybackAckTimeout != defaultPlaybackAckTimeout ||
		a.cfg.InterruptionTimeout != defaultInterruptionTimeout ||
		a.cfg.MaxOutcomeRecovery != defaultMaxOutcomeRecovery ||
		a.cfg.MaxIVRSteps != defaultMaxIVRSteps ||
		a.cfg.IVRWaitTimeout != defaultIVRWaitTimeout {
		t.Errorf("defaults not applied: %+v", a.cfg)
	}
	// Must exceed the C2 realtime TTS 30 s final-drain limit and the STT 15 s limit.
	if a.cfg.CleanupTimeout < 35*time.Second {
		t.Errorf("default cleanup timeout = %v, want >= 35s", a.cfg.CleanupTimeout)
	}

	missing := root
	missing.Dialogue = nil
	if _, err := New(missing); err == nil {
		t.Error("missing dialogue accepted")
	}
	inconsistent := root
	inconsistent.VAD.EndSilenceFrames = defaultMaxUtteranceFrames + 1
	if _, err := New(inconsistent); err == nil {
		t.Error("inconsistent silence/max accepted")
	}
	startTooLarge := root
	startTooLarge.VAD.StartFrames = 100
	if _, err := New(startTooLarge); err == nil {
		t.Error("start > pre-roll accepted")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.VAD.PreRollFrames = -1 },
		func(c *Config) { c.Hold.WindowFrames = -1 },
		func(c *Config) { c.ListenTimeout = -1 },
		func(c *Config) { c.MaxUserTurns = -1 },
		func(c *Config) { c.MaxToolRounds = -1 },
		func(c *Config) { c.MaxCallDuration = -1 },
		func(c *Config) { c.CleanupTimeout = -1 },
		func(c *Config) { c.AssistantTurnTimeout = -1 },
		func(c *Config) { c.PlaybackAckTimeout = -1 },
		func(c *Config) { c.InterruptionTimeout = -1 },
		func(c *Config) { c.MaxOutcomeRecovery = -1 },
		func(c *Config) { c.MaxIVRSteps = -1 },
		func(c *Config) { c.IVRWaitTimeout = -1 },
	} {
		cfg := root
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("negative config accepted: %+v", cfg)
		}
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	a := fastAgent(t, testConfig(newFakeStore()))
	if _, err := a.Run(context.Background(), testOrder(), nil, nil, nil); err == nil {
		t.Error("nil audio session accepted")
	}
	order := testOrder()
	order.ID = ""
	if _, err := a.Run(context.Background(), order, newFakeCall(), nil, nil); err == nil {
		t.Error("empty order id accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Run(ctx, testOrder(), newFakeCall(), nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx err = %v", err)
	}
}

func TestRunGreetingThenUserTurnTextOnly(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Guten Tag, hier ist die Ordination."}}}
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("Einen Moment bitte.")
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	a := fastAgent(t, cfg)

	call := newFakeCall()
	ended := make(chan domain.CallEvent)
	done := startAgent(t, a, order, call, ended)

	// Wait until both greeting segments were confirmed, then speak.
	waitFor(t, "greeting confirmed", func() bool {
		call.mu.Lock()
		n := len(call.marks)
		call.mu.Unlock()
		return n >= 2
	})
	call.push(utteranceAudio()...)
	waitFor(t, "reply fed to TTS", func() bool {
		tts.mu.Lock()
		defer tts.mu.Unlock()
		return len(tts.texts) >= 1 && strings.Contains(strings.Join(tts.texts, ""), "Einen Moment")
	})
	close(ended)
	if _, err := resultOf(t, done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v, want ErrCallEnded", err)
	}
	if stt.opens != 1 {
		t.Errorf("STT opens = %d, want 1", stt.opens)
	}
	if dlg.opens != 1 || dlg.closed != 1 {
		t.Errorf("dialogue opens/closes = %d/%d", dlg.opens, dlg.closed)
	}
	dlg.mu.Lock()
	texts := append([]string(nil), dlg.texts...)
	req := dlg.requests[0]
	dlg.mu.Unlock()
	if len(texts) != 1 || !strings.Contains(texts[0], "Ordination") {
		t.Errorf("user turn = %q", texts)
	}
	if req.OrderID != order.ID || len(req.Tools) != 4 {
		t.Errorf("session request = %+v", req)
	}
	if !strings.Contains(req.SystemPrompt, "Auftragsdaten") {
		t.Error("system prompt not passed")
	}
	// Greeting was confirmed, so the prompt must not ask to repair the disclosure.
	if strings.Contains(req.SystemPrompt, "Begrüßung wurde unterbrochen") {
		t.Error("prompt requires disclosure repair after a confirmed greeting")
	}
	if string(req.OutcomeSchema) != string(OutcomeSchema()) {
		t.Error("outcome schema not passed")
	}
	if call.sentCount() == 0 {
		t.Error("no outbound audio")
	}
}

func TestRunGreetingBargeUpdatesDisclosurePrompt(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("Wie kann ich helfen?")
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"Hallo"}}}
	cfg.TTS, cfg.Dialogue = tts, dlg
	a := fastAgent(t, cfg)

	// Hold the greeting's first synthesis open so inbound speech arrives while assistant
	// audio is actually being generated; the onset must be published before the second
	// segment mark, so the disclosure is not confirmed.
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tts.feedHook = func(string) {
		once.Do(func() {
			close(blocked)
			<-release
		})
	}
	call := newFakeCall()
	ended := make(chan domain.CallEvent)
	done := startAgent(t, a, order, call, ended)
	<-blocked
	call.push(utteranceAudio()...)
	time.Sleep(80 * time.Millisecond)
	close(release)
	waitFor(t, "dialogue opens after barge", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return dlg.opens == 1
	})
	close(ended)
	_, _ = resultOf(t, done)
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.requests) != 1 {
		t.Fatalf("session requests = %d", len(dlg.requests))
	}
	prompt := dlg.requests[0].SystemPrompt
	if !strings.Contains(prompt, "Begrüßung wurde unterbrochen") {
		t.Error("interrupted greeting did not produce a disclosure/purpose repair prompt")
	}
	if call.clearCount() == 0 {
		t.Error("barge-in did not clear queued greeting audio")
	}
}

func TestRunBookedToolLoop(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Am 15. Jänner um 9 Uhr ist möglich."}}}
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A"}`
	confirm := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A","confirmation_code":"TEST-123"}`
	booked := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "Termin bestätigt",
		BookedSlot: &domain.BookedSlot{DateTime: slot},
	}
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Einen Moment."}, result: ports.TurnResult{ToolCalls: []ports.ToolCall{
			{ID: "c1", Name: ToolProposeSlot, Arguments: []byte(propose)},
		}}},
		{result: ports.TurnResult{ToolCalls: []ports.ToolCall{
			{ID: "c2", Name: ToolConfirmBooking, Arguments: []byte(confirm)},
		}}},
		{chunks: []string{"Gerne gebucht."}, result: ports.TurnResult{Outcome: &booked}},
	}}
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, &fakeTTS{}, dlg
	a := fastAgent(t, cfg)

	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	outcome, err := resultOf(t, done)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.BookedSlot == nil || !outcome.BookedSlot.DateTime.Equal(slot) {
		t.Errorf("booked slot = %+v", outcome.BookedSlot)
	}
	if outcome.BookedSlot.Doctor != "Dr. A" || outcome.BookedSlot.Practice != "Praxis A" {
		t.Errorf("canonicalized slot = %+v", outcome.BookedSlot)
	}

	dlg.mu.Lock()
	results := append([][]ports.ToolResult(nil), dlg.results...)
	texts := append([]string(nil), dlg.texts...)
	dlg.mu.Unlock()
	if len(texts) != 1 {
		t.Errorf("user turns = %q", texts)
	}
	if len(results) != 2 {
		t.Fatalf("tool-result submissions = %d, want 2", len(results))
	}
	if results[0][0].ID != "c1" || string(results[0][0].Result) != `{"ok":true,"start":"2027-01-15T09:00:00+01:00","end":"2027-01-15T09:30:00+01:00"}` {
		t.Errorf("propose result = %+v", results[0])
	}
	if results[1][0].ID != "c2" || string(results[1][0].Result) != `{"ok":true,"confirmation_code":"TEST-123"}` {
		t.Errorf("confirm result = %+v", results[1])
	}
}

func TestRunOutcomeWithoutConfirmationRepairs(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	out := domain.Outcome{Kind: domain.OutcomeBooked, Detail: "booked", BookedSlot: &domain.BookedSlot{DateTime: slot}}
	repair := domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "other", Detail: "nicht bestätigt"}
	dlg := &fakeDialogue{scripts: []turnScript{
		{result: ports.TurnResult{Outcome: &out}},
		{chunks: []string{"Das konnte ich nicht bestätigen."}, result: ports.TurnResult{Outcome: &repair}},
	}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	outcome, err := resultOf(t, done)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != domain.OutcomeFailed {
		t.Fatalf("outcome = %+v", outcome)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.controls) != 1 || !strings.Contains(dlg.controls[0], "tatsächlich ausgeführten Werkzeugen") {
		t.Errorf("recovery controls = %q", dlg.controls)
	}
	if len(dlg.interrupts) != 1 {
		t.Errorf("interrupts = %d, want 1 rollback", len(dlg.interrupts))
	}
}

func TestRunToolRoundLimit(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `"}`
	tool := ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolProposeSlot, Arguments: []byte(propose)}}}
	dlg := &fakeDialogue{scripts: []turnScript{{result: tool}, {result: tool}, {result: tool}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	cfg.MaxToolRounds = 2
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, errToolRoundsExceeded) {
		t.Fatalf("err = %v, want errToolRoundsExceeded", err)
	}
	if !IsLimit(err) {
		t.Fatalf("tool-round limit not classified as a conversation limit: %v", err)
	}
	dlg.mu.Lock()
	submissions := len(dlg.results)
	dlg.mu.Unlock()
	if submissions != 2 {
		t.Errorf("submissions = %d, want 2", submissions)
	}
}

func TestRunUserTurnLimit(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	dlg := textOnlyDialogue("ok")
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	cfg.MaxUserTurns = 1
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	waitFor(t, "first turn handled", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, errUserTurnsExceeded) {
		t.Fatalf("err = %v, want errUserTurnsExceeded", err)
	}
	if !IsLimit(err) {
		t.Fatalf("user-turn limit not classified as a conversation limit: %v", err)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.texts) != 1 {
		t.Errorf("user turns = %d, want 1", len(dlg.texts))
	}
}

func TestRunEmptyTranscriptPolicy(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{scripts: []fakeSTTScript{{finals: []string{""}}, {finals: []string{"   "}}}}
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("ok")
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	waitFor(t, "repeat prompt", func() bool {
		tts.mu.Lock()
		defer tts.mu.Unlock()
		for _, text := range tts.texts {
			if strings.Contains(text, "wiederholen") {
				return true
			}
		}
		return false
	})
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, errEmptyTranscript) {
		t.Fatalf("err = %v, want errEmptyTranscript", err)
	}
	if !IsLimit(err) {
		t.Fatalf("empty-transcript limit not classified as a conversation limit: %v", err)
	}
	if stt.opens != 2 {
		t.Errorf("STT opens = %d, want 2", stt.opens)
	}
}

func TestRunFinishCallNeverExecuted(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	dlg := &fakeDialogue{scripts: []turnScript{{result: ports.TurnResult{
		ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolFinishCall, Arguments: []byte(`{}`)}},
	}}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	call := newFakeCall()
	call.push(utteranceAudio()...)
	_, err := runAgent(t, a, order, call, nil)
	if err == nil {
		t.Fatal("finish_call as an ordinary tool call was executed")
	}
}

func TestRunToolAndOutcomeErrorsAbort(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	execErr := errors.New("tool storage down")
	toolCall := ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolCaptureInfo, Arguments: []byte(`{}`)}}}
	dlg := &fakeDialogue{scripts: []turnScript{{result: toolCall}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	cfg.NewTools = func(domain.Order) ToolExecutor { return &fakeToolExecutor{execErr: execErr} }
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	if _, err := resultOf(t, done); !errors.Is(err, execErr) {
		t.Fatalf("err = %v, want tool error", err)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if dlg.closed != 1 {
		t.Errorf("dialogue session not closed: %d", dlg.closed)
	}
}

func TestRunDialogueAndTTSFailures(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	openErr := errors.New("dialogue down")
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = &fakeDialogue{failOpen: openErr}
	call := newFakeCall()
	done := startAgent(t, fastAgent(t, cfg), order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	if _, err := resultOf(t, done); !errors.Is(err, openErr) {
		t.Fatalf("dialogue open err = %v", err)
	}

	userErr := errors.New("user turn failed")
	cfg = testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = &fakeDialogue{scripts: []turnScript{{err: userErr}}}
	call = newFakeCall()
	done = startAgent(t, fastAgent(t, cfg), order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	if _, err := resultOf(t, done); !errors.Is(err, userErr) {
		t.Fatalf("SendUserTurn err = %v", err)
	}

	ttsErr := errors.New("tts down")
	cfg = testConfig(store)
	cfg.TTS = &fakeTTS{script: fakeTTSScript{openErr: ttsErr}}
	if _, err := runAgent(t, fastAgent(t, cfg), order, newFakeCall(), nil); !errors.Is(err, ttsErr) {
		t.Fatalf("greeting TTS err = %v", err)
	}
}

func TestRunCallEndCancellationAndInboundClose(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)

	ended := make(chan domain.CallEvent)
	close(ended)
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = textOnlyDialogue("ok")
	if _, err := runAgent(t, fastAgent(t, cfg), order, newFakeCall(), ended); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("ended err = %v, want ErrCallEnded", err)
	}

	call := newFakeCall()
	call.closeInbound()
	cfg = testConfig(store)
	cfg.Dialogue = textOnlyDialogue("ok")
	if _, err := runAgent(t, fastAgent(t, cfg), order, call, nil); !errors.Is(err, errInboundClosed) {
		t.Errorf("inbound close err = %v, want errInboundClosed", err)
	}

	// External cancellation while listening returns the context error.
	ctx, cancel := context.WithCancel(context.Background())
	tts := &fakeTTS{}
	greeted := make(chan struct{})
	var once sync.Once
	tts.feedHook = func(string) { once.Do(func() { close(greeted) }) }
	cfg = testConfig(store)
	cfg.TTS = tts
	cfg.Dialogue = textOnlyDialogue("ok")
	done := make(chan error, 1)
	call = newFakeCall()
	go func() {
		_, err := fastAgent(t, cfg).Run(ctx, order, call, nil, call.Control())
		done <- err
	}()
	<-greeted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err = %v, want context.Canceled", err)
	}
}

func TestRunListenTimeout(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	cfg := testConfig(store)
	cfg.Dialogue = textOnlyDialogue("ok")
	cfg.ListenTimeout = 20 * time.Millisecond
	if _, err := runAgent(t, fastAgent(t, cfg), order, newFakeCall(), nil); !errors.Is(err, errListenTimeout) {
		t.Fatalf("err = %v, want listen timeout", err)
	} else if !IsLimit(err) {
		t.Fatalf("listen timeout not classified as a conversation limit: %v", err)
	}
}

func TestRunMaxCallDuration(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	cfg := testConfig(store)
	cfg.Dialogue = textOnlyDialogue("ok")
	cfg.MaxCallDuration = 200 * time.Millisecond
	cfg.ListenTimeout = 5 * time.Second
	_, err := runAgent(t, fastAgent(t, cfg), order, newFakeCall(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	} else if !IsLimit(err) {
		t.Fatalf("agent max-call deadline not classified as a conversation limit: %v", err)
	}
}

func TestRunProviderDeadlineIsNotConversationLimit(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{openErr: context.DeadlineExceeded}}
	cfg.Dialogue = textOnlyDialogue("ok")
	call := newFakeCall()
	done := startAgent(t, fastAgent(t, cfg), order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if IsLimit(err) {
		t.Fatal("a provider deadline must not be classified as an agent conversation limit")
	}
}

func TestExecuteToolsRejectsInvalidJSON(t *testing.T) {
	a := fastAgent(t, testConfig(newFakeStore()))
	_, err := a.executeTools(context.Background(), invalidJSONExecutor{}, []ports.ToolCall{{ID: "c", Name: ToolCaptureInfo}})
	if err == nil {
		t.Fatal("invalid tool JSON accepted")
	}
}

type invalidJSONExecutor struct{}

func (invalidJSONExecutor) Execute(context.Context, ports.ToolCall) ([]byte, error) {
	return []byte("not json"), nil
}

func (invalidJSONExecutor) ValidateOutcome(*domain.Outcome) error { return nil }

func TestExecuteToolsOrderAndIDs(t *testing.T) {
	a := fastAgent(t, testConfig(newFakeStore()))
	tools := &fakeToolExecutor{}
	calls := []ports.ToolCall{
		{ID: "a", Name: ToolCaptureInfo, Arguments: []byte(`{}`)},
		{ID: "b", Name: ToolCaptureInfo, Arguments: []byte(`{}`)},
	}
	results, err := a.executeTools(context.Background(), tools, calls)
	if err != nil {
		t.Fatalf("executeTools: %v", err)
	}
	if len(results) != 2 || results[0].ID != "a" || results[1].ID != "b" {
		t.Errorf("results = %+v", results)
	}
	if !json.Valid(results[0].Result) {
		t.Error("tool result is not valid JSON")
	}
}

func TestRunSilentSynthesisDoesNotUnlockTools(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `"}`
	dlg := &fakeDialogue{scripts: []turnScript{{
		chunks: []string{"Einen Moment."},
		result: ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c1", Name: ToolProposeSlot, Arguments: []byte(propose)}}},
	}}}
	tools := &fakeToolExecutor{}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"bitte buchen"}}}
	cfg.Dialogue = dlg
	cfg.TTS = &fakeTTS{script: fakeTTSScript{suppressText: "Einen Moment."}}
	cfg.NewTools = func(domain.Order) ToolExecutor { return tools }
	a := fastAgent(t, cfg)
	call := newFakeCall()
	done := startAgent(t, a, order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, errNoPlaybackAudio) {
		t.Fatalf("err = %v, want errNoPlaybackAudio", err)
	}
	if len(tools.executed) != 0 {
		t.Fatalf("silent assistant text unlocked tools: %+v", tools.executed)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if len(dlg.results) != 0 {
		t.Errorf("silent assistant text submitted tool results: %d", len(dlg.results))
	}
}

func TestRunCleanupBudgetIsFreshAtExit(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	dlg := textOnlyDialogue("ok")
	cfg := testConfig(store)
	cfg.Dialogue = dlg
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"hallo"}}}
	cfg.CleanupTimeout = 40 * time.Millisecond
	cfg.InterruptionTimeout = 10 * time.Millisecond
	cfg.MaxCallDuration = 250 * time.Millisecond
	cfg.ListenTimeout = 5 * time.Second
	call := newFakeCall()
	done := startAgent(t, fastAgent(t, cfg), order, call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	waitFor(t, "dialogue session created", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return dlg.opens == 1
	})
	_, err := resultOf(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the call deadline", err)
	}
	dlg.mu.Lock()
	defer dlg.mu.Unlock()
	if dlg.closed != 1 {
		t.Fatalf("dialogue closes = %d, want 1", dlg.closed)
	}
	if dlg.closeCtxErr != nil {
		t.Fatalf("dialogue close received an expired cleanup context: %v", dlg.closeCtxErr)
	}
}
