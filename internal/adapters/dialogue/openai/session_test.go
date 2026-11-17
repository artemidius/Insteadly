package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func newSession(t *testing.T, a *Adapter, tools []domain.Tool, schema []byte, prompt string) *session {
	t.Helper()
	sess, err := a.NewSession(context.Background(), ports.SessionRequest{
		OrderID:       domain.NewOrderID(),
		SystemPrompt:  prompt,
		Tools:         tools,
		OutcomeSchema: schema,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess.(*session)
}

func drainTurn(turn ports.AssistantTurn) (ports.TurnResult, string, error) {
	var text strings.Builder
	for c := range turn.TextChunks() {
		text.WriteString(c)
	}
	res, err := turn.Wait(context.Background())
	return res, text.String(), err
}

func sseChunk(delta map[string]any, finishReason string) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason == "" {
		choice["finish_reason"] = nil
	} else {
		choice["finish_reason"] = finishReason
	}
	b, _ := json.Marshal(map[string]any{
		"id":      "chat_1",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{choice},
	})
	return "data: " + string(b) + "\n\n"
}

// toolCallSSE builds a fragmented tool-call stream: the arguments string is split across
// two deltas (the second omitting id/name), then finished with the given reason.
func toolCallSSE(name, id, args, finishReason string) string {
	mid := len(args) / 2
	out := sseChunk(map[string]any{"tool_calls": []map[string]any{{
		"index":    0,
		"id":       id,
		"type":     "function",
		"function": map[string]any{"name": name, "arguments": args[:mid]},
	}}}, "")
	out += sseChunk(map[string]any{"tool_calls": []map[string]any{{
		"index":    0,
		"function": map[string]any{"arguments": args[mid:]},
	}}}, "")
	out += sseChunk(map[string]any{}, finishReason)
	out += "data: [DONE]\n\n"
	return out
}

func TestNewSessionValidation(t *testing.T) {
	var calls atomic.Int32
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})

	if _, err := a.NewSession(context.Background(), ports.SessionRequest{OutcomeSchema: outcomeSchema()}); err == nil {
		t.Error("empty order id must error")
	}
	if _, err := a.NewSession(context.Background(), ports.SessionRequest{
		OrderID: domain.NewOrderID(), OutcomeSchema: []byte(`{"type":"string"}`),
	}); err == nil {
		t.Error("non-object outcome schema must error")
	}
	if _, err := a.NewSession(context.Background(), ports.SessionRequest{
		OrderID: domain.NewOrderID(), OutcomeSchema: outcomeSchema(),
		Tools: []domain.Tool{{Name: "finish_call", Parameters: []byte(`{"type":"object"}`)}},
	}); err == nil {
		t.Error("reserved finish_call tool must error")
	}
	if _, err := a.NewSession(context.Background(), ports.SessionRequest{
		OrderID: domain.NewOrderID(), OutcomeSchema: outcomeSchema(),
		Tools: []domain.Tool{{Name: "Bad_Name", Parameters: []byte(`{"type":"object"}`)}},
	}); err == nil {
		t.Error("invalid tool name must error")
	}
	if calls.Load() != 0 {
		t.Fatal("NewSession must not make an HTTP call")
	}
}

func TestNewSessionPrompt(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	if !strings.Contains(s.prompt, "Insteadly") || !strings.Contains(s.prompt, "finish_call") {
		t.Errorf("default prompt missing expected content: %q", s.prompt)
	}
	s2 := newSession(t, a, nil, outcomeSchema(), "Custom prompt.")
	if !strings.HasPrefix(s2.prompt, "Custom prompt.") || !strings.Contains(s2.prompt, "finish_call") {
		t.Errorf("custom prompt not preserved with protocol sentence: %q", s2.prompt)
	}
}

func TestSendUserTurnStreamsText(t *testing.T) {
	a, c := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"Guten", " Tag."}, "stop"))
	})
	s := newSession(t, a, []domain.Tool{captureInfoTool()}, outcomeSchema(), "")

	turn, err := s.SendUserTurn(context.Background(), "Hallo")
	if err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	res, text, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if text != "Guten Tag." {
		t.Errorf("text = %q", text)
	}
	if len(res.ToolCalls) != 0 || res.Outcome != nil {
		t.Errorf("result = %+v", res)
	}

	auth, body := c.get()
	if auth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q", auth)
	}
	if body.Model != defaultModel || !body.Stream || body.ToolChoice != "auto" || body.ParallelToolCalls {
		t.Errorf("request options = %+v", body)
	}
	if body.Temperature != 0.1 || body.MaxCompletionTokens != 512 {
		t.Errorf("temperature/max_completion_tokens = %v/%v", body.Temperature, body.MaxCompletionTokens)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" || body.Messages[1].Content != "Hallo" {
		t.Errorf("messages = %+v", body.Messages)
	}
	if len(body.Tools) != 2 || body.Tools[1].Function.Name != "finish_call" {
		t.Errorf("tools = %+v", body.Tools)
	}
}

func TestSendUserTurnToolCallThenResults(t *testing.T) {
	var mu sync.Mutex
	phase := 0
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := phase
		phase++
		mu.Unlock()
		if p == 0 {
			writeSSE(w, toolCallSSE("capture_info", "call_1", `{"field":"reason","value":"Kontrolle"}`, "tool_calls"))
		} else {
			writeSSE(w, textSSE([]string{"ok"}, "stop"))
		}
	})

	s := newSession(t, a, []domain.Tool{captureInfoTool()}, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "Kontrolle bitte")
	res, _, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "capture_info" {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	if string(res.ToolCalls[0].Arguments) != `{"field":"reason","value":"Kontrolle"}` {
		t.Errorf("arguments = %s", res.ToolCalls[0].Arguments)
	}

	turn2, err := s.SubmitToolResults(context.Background(), []ports.ToolResult{{ID: "call_1", Result: []byte(`{"ok":true}`)}})
	if err != nil {
		t.Fatalf("SubmitToolResults: %v", err)
	}
	res2, text2, err := drainTurn(turn2)
	if err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	if text2 != "ok" || len(res2.ToolCalls) != 0 {
		t.Errorf("second result = %+v text=%q", res2, text2)
	}

	s.mu.Lock()
	messages := append([]chatMessage(nil), s.messages...)
	s.mu.Unlock()
	if len(messages) != 5 { // system, user, assistant(tool_calls), tool, assistant(text)
		t.Fatalf("committed messages = %d, want 5: %+v", len(messages), messages)
	}
	if messages[1].Role != "user" || messages[2].Role != "assistant" || len(messages[2].ToolCalls) != 1 {
		t.Errorf("history roles = %+v", messages)
	}
	if messages[3].Role != "tool" || messages[3].Name != "capture_info" || messages[3].ToolCallID != "call_1" || messages[3].Content != `{"ok":true}` {
		t.Errorf("tool message = %+v", messages[3])
	}
	if messages[4].Role != "assistant" || messages[4].Content != "ok" {
		t.Errorf("continuation assistant = %+v", messages[4])
	}
}

func TestSessionRollbackOnFailure(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"boom"}`))
			return
		}
		writeSSE(w, textSSE([]string{"good"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")

	turn, _ := s.SendUserTurn(context.Background(), "first")
	_, _, err := drainTurn(turn)
	if err == nil {
		t.Fatal("expected error for failed turn")
	}
	s.mu.Lock()
	if len(s.messages) != 1 {
		t.Fatalf("history should be unchanged (system only): %+v", s.messages)
	}
	s.mu.Unlock()

	fail.Store(false)
	turn2, _ := s.SendUserTurn(context.Background(), "second")
	_, text, err := drainTurn(turn2)
	if err != nil || text != "good" {
		t.Fatalf("second turn: text=%q err=%v", text, err)
	}
	s.mu.Lock()
	// C4 keeps the failed turn's triggering user message until a retry succeeds, so the
	// next ordinary user turn builds on it rather than silently losing it.
	msgs := append([]chatMessage(nil), s.messages...)
	s.mu.Unlock()
	if len(msgs) != 4 { // system, user(first), user(second), assistant
		t.Fatalf("history = %+v", msgs)
	}
	if msgs[1].Content != "first" || msgs[2].Content != "second" {
		t.Errorf("failed user turn was not preserved: %+v", msgs)
	}
}

func TestSessionInterruptAfterFailedTurn(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	a, capture := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeSSE(w, textSSE([]string{"ok"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")

	turn, _ := s.SendUserTurn(context.Background(), "first")
	if _, _, err := drainTurn(turn); err == nil {
		t.Fatal("expected failed turn")
	}
	if err := s.Interrupt(context.Background(), "Teilweise gehört"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	s.mu.Lock()
	msgs := append([]chatMessage(nil), s.messages...)
	s.mu.Unlock()
	if len(msgs) != 3 {
		t.Fatalf("history = %+v", msgs)
	}
	if msgs[1].Role != "user" || msgs[1].Content != "first" {
		t.Errorf("triggering user history lost: %+v", msgs)
	}
	if msgs[2].Role != "assistant" || msgs[2].Content != "Teilweise gehört" {
		t.Errorf("confirmed assistant text = %+v", msgs[2])
	}

	fail.Store(false)
	control, err := s.SendControl(context.Background(), "repair instruction")
	if err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if _, _, err := drainTurn(control); err != nil {
		t.Fatalf("control turn: %v", err)
	}
	_, body := capture.get()
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "system" || last.Content != "repair instruction" {
		t.Errorf("control message = %+v, want system role", last)
	}
	for _, m := range body.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "repair instruction") {
			t.Fatalf("control instruction was represented as user speech")
		}
	}
	if body.Messages[1].Content != "first" || body.Messages[2].Content != "Teilweise gehört" {
		t.Errorf("control candidate lost history: %+v", body.Messages)
	}
}

func TestSessionInterruptAfterCommittedTurn(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"very long unconfirmed answer"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "frage")
	if _, _, err := drainTurn(turn); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if err := s.Interrupt(context.Background(), "nur dieser Satz"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) != 3 {
		t.Fatalf("history = %+v", s.messages)
	}
	if s.messages[1].Content != "frage" {
		t.Errorf("user turn lost: %+v", s.messages)
	}
	if s.messages[2].Content != "nur dieser Satz" {
		t.Errorf("unconfirmed full text retained: %+v", s.messages[2])
	}
}

func TestSessionInterruptReopensFinished(t *testing.T) {
	var calls atomic.Int32
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeSSE(w, toolCallSSE("finish_call", "call_f", `{"kind":"aborted","detail":"done"}`, "tool_calls"))
			return
		}
		writeSSE(w, textSSE([]string{"weiter"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "abbrechen")
	res, _, err := drainTurn(turn)
	if err != nil || res.Outcome == nil {
		t.Fatalf("outcome turn: res=%+v err=%v", res, err)
	}
	if err := s.Interrupt(context.Background(), "kurz"); err != nil {
		t.Fatalf("Interrupt on finished session: %v", err)
	}
	s.mu.Lock()
	finished := s.finished
	s.mu.Unlock()
	if finished {
		t.Fatal("Interrupt did not reopen the finished session")
	}
	turn2, err := s.SendUserTurn(context.Background(), "noch etwas")
	if err != nil {
		t.Fatalf("SendUserTurn after reopen: %v", err)
	}
	if _, _, err := drainTurn(turn2); err != nil {
		t.Fatalf("post-reopen turn: %v", err)
	}
}

func TestSessionInterruptWhilePending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	if _, err := s.SendUserTurn(context.Background(), "eins"); err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	<-started
	if err := s.Interrupt(context.Background(), ""); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("Interrupt during turn = %v, want ErrTurnInProgress", err)
	}
	close(release)
}

func TestSessionSendControlReopensFinishedAndDiscardsOutcome(t *testing.T) {
	var calls atomic.Int32
	a, capture := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeSSE(w, toolCallSSE("finish_call", "call_f", `{"kind":"aborted","detail":"done"}`, "tool_calls"))
			return
		}
		writeSSE(w, textSSE([]string{"repariert"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "abbrechen")
	if _, _, err := drainTurn(turn); err != nil {
		t.Fatalf("turn: %v", err)
	}
	control, err := s.SendControl(context.Background(), "outcome reparieren")
	if err != nil {
		t.Fatalf("SendControl on finished: %v", err)
	}
	if _, _, err := drainTurn(control); err != nil {
		t.Fatalf("control turn: %v", err)
	}
	_, body := capture.get()
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "system" {
		t.Errorf("control role = %q, want system", last.Role)
	}
	for _, m := range body.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			t.Fatalf("rejected finish_call assistant message survived rollback: %+v", m)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		t.Error("control turn left the session finished")
	}
}

func TestSessionOneTurnAtATime(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")

	if _, err := s.SendUserTurn(context.Background(), "one"); err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	<-started
	if _, err := s.SendUserTurn(context.Background(), "two"); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("second SendUserTurn = %v, want ErrTurnInProgress", err)
	}
	close(release)
}

func TestSessionFinishedAfterOutcome(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, toolCallSSE("finish_call", "call_f", `{"kind":"aborted","detail":"done"}`, "tool_calls"))
	})
	s := newSession(t, a, []domain.Tool{captureInfoTool()}, outcomeSchema(), "")

	turn, _ := s.SendUserTurn(context.Background(), "abbrechen")
	res, _, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Outcome == nil || res.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("outcome = %+v", res.Outcome)
	}
	if _, err := s.SendUserTurn(context.Background(), "more"); !errors.Is(err, ErrSessionFinished) {
		t.Fatalf("SendUserTurn after finish = %v, want ErrSessionFinished", err)
	}
	if _, err := s.SubmitToolResults(context.Background(), nil); !errors.Is(err, ErrSessionFinished) {
		t.Fatalf("SubmitToolResults after finish = %v, want ErrSessionFinished", err)
	}
}

func TestSessionBlankInput(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {})
	s := newSession(t, a, nil, outcomeSchema(), "")
	if _, err := s.SendUserTurn(context.Background(), "   "); err == nil {
		t.Error("blank input must error")
	}
}

func TestToolResultValidation(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, toolCallSSE("capture_info", "call_1", `{"a":1}`, "tool_calls"))
	})
	s := newSession(t, a, []domain.Tool{captureInfoTool()}, outcomeSchema(), "")

	if _, err := s.SubmitToolResults(context.Background(), []ports.ToolResult{{ID: "x", Result: []byte(`{}`)}}); err == nil {
		t.Error("SubmitToolResults with no pending calls must error")
	}

	turn, _ := s.SendUserTurn(context.Background(), "hi")
	_, _, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	cases := []struct {
		name    string
		results []ports.ToolResult
	}{
		{"missing", []ports.ToolResult{{ID: "other", Result: []byte(`{}`)}}},
		{"wrong count", []ports.ToolResult{{ID: "call_1", Result: []byte(`{}`)}, {ID: "call_2", Result: []byte(`{}`)}}},
		{"unknown id", []ports.ToolResult{{ID: "nope", Result: []byte(`{}`)}}},
		{"invalid json", []ports.ToolResult{{ID: "call_1", Result: []byte(`not-json`)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SubmitToolResults(context.Background(), tc.results); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSessionCloseIdempotent(t *testing.T) {
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.SendUserTurn(context.Background(), "x"); !errors.Is(err, ErrSessionFinished) {
		t.Errorf("SendUserTurn after Close = %v, want ErrSessionFinished", err)
	}
}

func TestSessionCloseCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, _ := s.SendUserTurn(context.Background(), "x")
	<-started
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, err := drainTurn(turn)
	if err == nil {
		t.Error("in-flight turn must error after Close cancels it")
	}
}

// TestRequestBodyUsesMaxCompletionTokens pins the OpenAI-specific request field. The
// Mistral adapter sends max_tokens; OpenAI deprecates it for current models, so the
// alternative provider must send max_completion_tokens and never max_tokens.
func TestRequestBodyUsesMaxCompletionTokens(t *testing.T) {
	a, c := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"ok"}, "stop"))
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, err := s.SendUserTurn(context.Background(), "hallo")
	if err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	if _, _, err := drainTurn(turn); err != nil {
		t.Fatalf("turn: %v", err)
	}
	raw := c.raw()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if _, ok := fields["max_tokens"]; ok {
		t.Fatal("request body still sends max_tokens")
	}
	if _, ok := fields["max_completion_tokens"]; !ok {
		t.Fatal("request body does not send max_completion_tokens")
	}
	var got int
	if err := json.Unmarshal(fields["max_completion_tokens"], &got); err != nil || got != 512 {
		t.Fatalf("max_completion_tokens = %d err %v, want 512", got, err)
	}
}
