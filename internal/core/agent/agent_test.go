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

func utteranceChunks() [][]byte {
	return flatten(frames(10, pcmFrame(2000)), frames(30, silenceFrame()))
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	root := testConfig(newFakeStore())
	root.STT = &fakeSTT{}
	a := fastAgent(t, root)
	if a.cfg.SpeechThreshold != defaultSpeechThreshold ||
		a.cfg.MaxUserTurns != defaultMaxUserTurns ||
		a.cfg.MaxToolRounds != defaultMaxToolRounds ||
		a.cfg.MaxCallDuration != defaultMaxCallDuration {
		t.Errorf("defaults not applied: %+v", a.cfg)
	}

	missing := root
	missing.Dialogue = nil
	if _, err := New(missing); err == nil {
		t.Error("missing dialogue accepted")
	}

	negative := root
	negative.SpeechThreshold = -1
	if _, err := New(negative); err == nil {
		t.Error("negative threshold accepted")
	}
	inconsistent := root
	inconsistent.EndSilenceFrames = defaultMaxUtteranceFrames + 1
	if _, err := New(inconsistent); err == nil {
		t.Error("inconsistent silence/max accepted")
	}
	startTooLarge := root
	startTooLarge.StartFrames = 100
	if _, err := New(startTooLarge); err == nil {
		t.Error("start > pre-roll accepted")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.PreRollFrames = -1 },
		func(c *Config) { c.StartFrames = -1 },
		func(c *Config) { c.MinActiveFrames = -1 },
		func(c *Config) { c.EndSilenceFrames = -1 },
		func(c *Config) { c.MaxUtteranceFrames = -1 },
		func(c *Config) { c.ListenTimeout = -1 },
		func(c *Config) { c.MaxUserTurns = -1 },
		func(c *Config) { c.MaxToolRounds = -1 },
		func(c *Config) { c.MaxCallDuration = -1 },
		func(c *Config) { c.ReplayPace = -1 },
		func(c *Config) { c.CleanupTimeout = -1 },
	} {
		cfg := root
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("negative config accepted")
		}
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	a := fastAgent(t, testConfig(newFakeStore()))
	if _, err := a.Run(context.Background(), testOrder(), nil, nil); err == nil {
		t.Error("nil audio session accepted")
	}
	order := testOrder()
	order.ID = ""
	if _, err := a.Run(context.Background(), order, newFakeCall(), nil); err == nil {
		t.Error("empty order id accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Run(ctx, testOrder(), newFakeCall(), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx err = %v", err)
	}
}

func TestRunGreetingThenListenThenTextOnly(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Guten Tag, hier ist die Ordination."}}}
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("Einen Moment bitte.")
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	a := fastAgent(t, cfg)

	call := newFakeCall(utteranceChunks()...)
	ended := make(chan domain.CallEvent)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			// Wait until the reply was actually fed to TTS, not merely requested from
			// dialogue: ending the call first would legitimately cancel the generation.
			tts.mu.Lock()
			done := len(tts.texts) == 2
			tts.mu.Unlock()
			if done {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(ended)
	}()

	outcome, err := a.Run(context.Background(), order, call, ended)
	if !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v, want ErrCallEnded", err)
	}
	if outcome.Kind != "" {
		t.Errorf("unexpected outcome %+v", outcome)
	}
	if stt.opens != 1 {
		t.Errorf("STT opens = %d, want 1", stt.opens)
	}
	if dlg.opens != 1 || dlg.closed != 1 {
		t.Errorf("dialogue opens/closes = %d/%d", dlg.opens, dlg.closed)
	}
	if len(dlg.texts) != 1 || !strings.Contains(dlg.texts[0], "Ordination") {
		t.Errorf("user turn = %q", dlg.texts)
	}
	if len(dlg.requests) != 1 {
		t.Fatalf("session requests = %d", len(dlg.requests))
	}
	req := dlg.requests[0]
	if req.OrderID != order.ID || len(req.Tools) != 4 {
		t.Errorf("session request = %+v", req)
	}
	if !strings.Contains(req.SystemPrompt, "Auftragsdaten") {
		t.Error("system prompt not passed")
	}
	if string(req.OutcomeSchema) != string(OutcomeSchema()) {
		t.Error("outcome schema not passed")
	}
	if tts.opens != 2 {
		t.Errorf("TTS opens = %d, want greeting + reply", tts.opens)
	}
	tts.mu.Lock()
	texts := append([]string(nil), tts.texts...)
	tts.mu.Unlock()
	if len(texts) != 2 || !strings.Contains(texts[0], order.Patient.FullName) {
		t.Errorf("TTS texts = %q", texts)
	}
	if call.sentCount() == 0 {
		t.Error("no outbound audio")
	}
}

func TestRunHalfDuplexNoSTTWhileSpeaking(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Hallo"}}}
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("Einen Moment.")
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	cfg.ListenTimeout = 150 * time.Millisecond
	a := fastAgent(t, cfg)

	var once sync.Once
	opened := make(chan struct{})
	release := make(chan struct{})
	tts.feedHook = func(text string) {
		if strings.Contains(text, "Guten Tag") {
			once.Do(func() { close(opened) })
			<-release
		}
	}

	call := newFakeCall(utteranceChunks()...)
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), order, call, nil)
		done <- err
	}()
	<-opened
	stt.mu.Lock()
	opensDuringGreeting := stt.opens
	stt.mu.Unlock()
	if opensDuringGreeting != 0 {
		t.Errorf("STT opened during greeting: %d", opensDuringGreeting)
	}
	close(release)
	if err := <-done; !errors.Is(err, errListenTimeout) {
		t.Fatalf("err = %v, want listen timeout", err)
	}
	if stt.opens != 1 {
		t.Errorf("STT opens after greeting = %d, want 1", stt.opens)
	}
}

func TestRunBookedToolLoop(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Am 15. Jänner um 9 Uhr ist möglich."}}}
	tts := &fakeTTS{}
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A"}`
	confirm := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A","confirmation_code":"TEST-123"}`
	booked := domain.Outcome{
		Kind:   domain.OutcomeBooked,
		Detail: "Termin bestätigt",
		BookedSlot: &domain.BookedSlot{
			DateTime: slot,
		},
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
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	a := fastAgent(t, cfg)

	call := newFakeCall(utteranceChunks()...)
	outcome, err := a.Run(context.Background(), order, call, nil)
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

	tts.mu.Lock()
	var fed []string
	fed = append(fed, tts.texts...)
	tts.mu.Unlock()
	if len(fed) != 3 || fed[1] != "Einen Moment." || fed[2] != "Gerne gebucht." {
		t.Errorf("spoken texts = %q", fed)
	}
}

func TestRunOutcomeWithoutConfirmationFails(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	slot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	out := domain.Outcome{Kind: domain.OutcomeBooked, Detail: "booked", BookedSlot: &domain.BookedSlot{DateTime: slot}}
	dlg := &fakeDialogue{scripts: []turnScript{{result: ports.TurnResult{Outcome: &out}}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	_, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil)
	if !errors.Is(err, domain.ErrInvalidOutcome) {
		t.Fatalf("err = %v, want ErrInvalidOutcome", err)
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
	_, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil)
	if !errors.Is(err, errToolRoundsExceeded) {
		t.Fatalf("err = %v, want errToolRoundsExceeded", err)
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
	call := newFakeCall(flatten(utteranceChunks(), utteranceChunks())...)
	_, err := a.Run(context.Background(), order, call, nil)
	if !errors.Is(err, errUserTurnsExceeded) {
		t.Fatalf("err = %v, want errUserTurnsExceeded", err)
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
	stt := &fakeSTT{scripts: []fakeSTTScript{
		{finals: []string{""}},
		{finals: []string{"   "}},
	}}
	tts := &fakeTTS{}
	dlg := textOnlyDialogue("ok")
	cfg := testConfig(store)
	cfg.STT, cfg.TTS, cfg.Dialogue = stt, tts, dlg
	a := fastAgent(t, cfg)
	call := newFakeCall(flatten(utteranceChunks(), utteranceChunks())...)
	_, err := a.Run(context.Background(), order, call, nil)
	if !errors.Is(err, errEmptyTranscript) {
		t.Fatalf("err = %v, want errEmptyTranscript", err)
	}
	if stt.opens != 2 {
		t.Errorf("STT opens = %d, want 2", stt.opens)
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if len(tts.texts) != 2 || tts.texts[1] != pleaseRepeatPrompt {
		t.Errorf("spoken = %q", tts.texts)
	}
}

func TestRunEmptyAssistantTurnFails(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	dlg := &fakeDialogue{scripts: []turnScript{{}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	_, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil)
	if !errors.Is(err, errEmptyTurn) {
		t.Fatalf("err = %v, want errEmptyTurn", err)
	}
}

func TestRunOutcomeTurnWithCallsFails(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	out := domain.Outcome{Kind: domain.OutcomeAborted, Detail: "aborted"}
	dlg := &fakeDialogue{scripts: []turnScript{{result: ports.TurnResult{
		Outcome:   &out,
		ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolCaptureInfo}},
	}}}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	if _, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil); err == nil {
		t.Fatal("outcome with tool calls accepted")
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
	if _, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil); err == nil {
		t.Fatal("finish_call as an ordinary tool call was executed")
	}
}

func TestRunToolAndOutcomeErrorsAbort(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	execErr := errors.New("tool storage down")
	validateErr := errors.New("outcome mismatch")
	toolCall := ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolCaptureInfo, Arguments: []byte(`{}`)}}}
	outcome := domain.Outcome{Kind: domain.OutcomeAborted, Detail: "aborted"}

	for name, tc := range map[string]struct {
		scripts []turnScript
		tools   ToolExecutor
		wantErr error
	}{
		"tool operational error": {
			scripts: []turnScript{{result: toolCall}},
			tools:   &fakeToolExecutor{execErr: execErr},
			wantErr: execErr,
		},
		"outcome validation error": {
			scripts: []turnScript{{result: ports.TurnResult{Outcome: &outcome}}},
			tools:   &fakeToolExecutor{validateErr: validateErr},
			wantErr: validateErr,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dlg := &fakeDialogue{scripts: tc.scripts}
			cfg := testConfig(store)
			cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
			cfg.Dialogue = dlg
			cfg.NewTools = func(domain.Order) ToolExecutor { return tc.tools }
			a := fastAgent(t, cfg)
			_, err := a.Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			dlg.mu.Lock()
			defer dlg.mu.Unlock()
			if dlg.closed != 1 {
				t.Errorf("dialogue session not closed: %d", dlg.closed)
			}
		})
	}
}

func TestRunDialogueAndTTSFailures(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	openErr := errors.New("dialogue down")
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = &fakeDialogue{failOpen: openErr}
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil); !errors.Is(err, openErr) {
		t.Fatalf("dialogue open err = %v", err)
	}

	userErr := errors.New("user turn failed")
	cfg = testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = &fakeDialogue{scripts: []turnScript{{err: userErr}}}
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil); !errors.Is(err, userErr) {
		t.Fatalf("SendUserTurn err = %v", err)
	}

	submitErr := errors.New("tool submission failed")
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `"}`
	cfg = testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = &fakeDialogue{scripts: []turnScript{
		{result: ports.TurnResult{ToolCalls: []ports.ToolCall{{ID: "c", Name: ToolProposeSlot, Arguments: []byte(propose)}}}},
		{err: submitErr},
	}}
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(utteranceChunks()...), nil); !errors.Is(err, submitErr) {
		t.Fatalf("SubmitToolResults err = %v", err)
	}

	ttsErr := errors.New("tts down")
	cfg = testConfig(store)
	cfg.TTS = &fakeTTS{script: fakeTTSScript{openErr: ttsErr}}
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(), nil); !errors.Is(err, ttsErr) {
		t.Fatalf("greeting TTS err = %v", err)
	}
}

func TestRunCallEndCancellationAndInboundClose(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)

	// Call end before any speech: the agent reports ErrCallEnded.
	ended := make(chan domain.CallEvent)
	close(ended)
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue = textOnlyDialogue("ok")
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(), ended); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("ended err = %v, want ErrCallEnded", err)
	}

	// Inbound closure without a terminal event is a provider-contract failure.
	call := newFakeCall()
	call.closeInbound()
	cfg = testConfig(store)
	cfg.Dialogue = textOnlyDialogue("ok")
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, call, nil); !errors.Is(err, errInboundClosed) {
		t.Fatalf("inbound close err = %v, want errInboundClosed", err)
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
	go func() {
		_, err := fastAgent(t, cfg).Run(ctx, order, newFakeCall(), nil)
		done <- err
	}()
	<-greeted
	waitFor(t, "greeting close", func() bool {
		tts.mu.Lock()
		defer tts.mu.Unlock()
		return tts.closes == 1
	})
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
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(), nil); !errors.Is(err, errListenTimeout) {
		t.Fatalf("err = %v, want listen timeout", err)
	}
}

func TestRunMaxCallDuration(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	cfg := testConfig(store)
	cfg.Dialogue = textOnlyDialogue("ok")
	cfg.MaxCallDuration = 20 * time.Millisecond
	cfg.ListenTimeout = time.Second
	if _, err := fastAgent(t, cfg).Run(context.Background(), order, newFakeCall(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestExecuteToolsRejectsInvalidJSON(t *testing.T) {
	a := fastAgent(t, testConfig(newFakeStore()))
	// A fake executor that returns invalid JSON must not reach SubmitToolResults.
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
