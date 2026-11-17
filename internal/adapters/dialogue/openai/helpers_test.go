package openai

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const testAPIKey = "test-openai-api-key"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{
		APIKey:     testAPIKey,
		BaseURL:    baseURL,
		HTTPClient: &http.Client{},
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// outcomeSchema is the exact §3.8 canonical outcome schema.
func outcomeSchema() []byte {
	return []byte(`{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["booked","callback_needed","failed","aborted"]},"detail":{"type":"string","minLength":1},"booked_slot":{"type":"object","additionalProperties":false,"properties":{"date_time":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["date_time"]},"callback_at":{"type":"string","format":"date-time"},"fail_reason":{"type":"string","minLength":1}},"required":["kind","detail"]}`)
}

func captureInfoTool() domain.Tool {
	return domain.Tool{
		Name:        "capture_info",
		Description: "Capture one item of patient information.",
		Parameters:  []byte(`{"type":"object","properties":{"field":{"type":"string"},"value":{"type":"string"}},"required":["field","value"]}`),
	}
}

type chatCapture struct {
	mu      sync.Mutex
	auth    string
	rawBody []byte
	body    chatRequest
}

func (c *chatCapture) get() (string, chatRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.body
}

func (c *chatCapture) raw() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.rawBody...)
}

func newChatServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Adapter, *chatCapture) {
	t.Helper()
	c := &chatCapture{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.auth = r.Header.Get("Authorization")
		c.rawBody, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(c.rawBody, &c.body)
		c.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return newTestAdapter(t, ts.URL), c
}

func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, body)
}

// textChunk is a helper to build one Chat Completions SSE chunk line.
func chunkLine(content, finishReason string) string {
	b, _ := json.Marshal(map[string]any{
		"id":      "chat_1",
		"object":  "chat.completion.chunk",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": finishReason}},
	})
	return "data: " + string(b) + "\n\n"
}

func textSSE(chunks []string, finishReason string) string {
	out := ""
	for _, c := range chunks {
		out += chunkLine(c, "")
	}
	out += chunkLine("", finishReason)
	out += "data: [DONE]\n\n"
	return out
}
