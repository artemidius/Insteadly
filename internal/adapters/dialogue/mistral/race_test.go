package mistral

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// TestNewSessionCopiesCallerSlices verifies that mutating the caller's tool Parameters and
// OutcomeSchema after NewSession does not change the session's stored tool definitions.
func TestNewSessionCopiesCallerSlices(t *testing.T) {
	tool := captureInfoTool()
	schema := outcomeSchema()
	origParams := string(tool.Parameters)
	origSchema := string(schema)

	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, textSSE([]string{"x"}, "stop"))
	})
	sess, err := a.NewSession(context.Background(), ports.SessionRequest{
		OrderID:       domain.NewOrderID(),
		Tools:         []domain.Tool{tool},
		OutcomeSchema: schema,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s := sess.(*session)

	for i := range tool.Parameters {
		tool.Parameters[i] = 0
	}
	for i := range schema {
		schema[i] = 0
	}

	if len(s.tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(s.tools))
	}
	if string(s.tools[0].Function.Parameters) != origParams {
		t.Error("caller tool Parameters were mutated through the caller's slice")
	}
	if string(s.tools[1].Function.Parameters) != origSchema {
		t.Error("outcome schema was mutated through the caller's slice")
	}
}

// TestTurnEmitInterruptible verifies a blocked text producer is interruptible by ctx/done.
func TestTurnEmitInterruptible(t *testing.T) {
	turn := newTurn()
	for i := 0; i < 64; i++ {
		turn.textChunks <- "x"
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := turn.emit(ctx, "blocked"); !errors.Is(err, context.Canceled) {
		t.Errorf("emit = %v, want context.Canceled", err)
	}
}

func TestFinalizeCallsMultipleCalls(t *testing.T) {
	calls := []assembledCall{
		{index: 0, id: "a", name: "capture_info", typ: "function", args: *sb(`{"field":"reason","value":"K"}`)},
		{index: 1, id: "b", name: "capture_info", typ: "function", args: *sb(`{"field":"note","value":"n"}`)},
	}
	assistant, res, err := finalizeCalls(chatMessage{Role: "assistant"}, "", calls, allowAll())
	if err != nil {
		t.Fatalf("finalizeCalls: %v", err)
	}
	if len(res.ToolCalls) != 2 || res.Outcome != nil {
		t.Fatalf("result = %+v", res)
	}
	if res.ToolCalls[0].ID != "a" || res.ToolCalls[1].ID != "b" {
		t.Errorf("tool call order = %+v", res.ToolCalls)
	}
	if len(assistant.ToolCalls) != 2 {
		t.Errorf("assistant tool calls = %+v", assistant.ToolCalls)
	}
}

// TestDialogueMultilineDataStreamsText verifies a chat chunk split across multiple data:
// lines is reassembled and streamed.
func TestDialogueMultilineDataStreamsText(t *testing.T) {
	// Split at a comma boundary so the joined JSON remains valid.
	chunk := `{"id":"chat_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`
	idx := strings.Index(chunk, `"choices"`)
	body := "data: " + chunk[:idx] + "\n" + "data: " + chunk[idx:] + "\n\n" + "data: [DONE]\n\n"

	a, _ := newChatServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, body)
	})
	s := newSession(t, a, nil, outcomeSchema(), "")
	turn, err := s.SendUserTurn(context.Background(), "hi")
	if err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	_, text, err := drainTurn(turn)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if text != "hello" {
		t.Errorf("text = %q, want hello", text)
	}
}
