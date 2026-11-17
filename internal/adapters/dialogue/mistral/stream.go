package mistral

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	maxAccumText = 64 << 10
	maxToolArgs  = 64 << 10
	maxCalls     = 8
)

// turn implements ports.AssistantTurn. The response reader is the only producer of
// TextChunks; it closes textChunks before making Wait ready.
type turn struct {
	textChunks chan string
	done       chan struct{}

	mu     sync.Mutex
	result ports.TurnResult
	err    error
}

var _ ports.AssistantTurn = (*turn)(nil)

func newTurn() *turn {
	return &turn{textChunks: make(chan string, 64), done: make(chan struct{})}
}

func (t *turn) TextChunks() <-chan string { return t.textChunks }

func (t *turn) emit(text string) { t.textChunks <- text }

func (t *turn) finish(res ports.TurnResult, err error) {
	t.mu.Lock()
	t.result = res
	t.err = err
	t.mu.Unlock()
	close(t.textChunks)
	close(t.done)
}

func (t *turn) Wait(ctx context.Context) (ports.TurnResult, error) {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.result, t.err
	case <-ctx.Done():
		return ports.TurnResult{}, ctx.Err()
	}
}

// --- SSE chunk shapes ---

type chatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []deltaToolCall `json:"tool_calls"`
}

type deltaToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chat performs one Chat Completions request, streaming text into t and returning the
// assistant message (for history) plus the assembled result.
func (a *Adapter) chat(ctx context.Context, msgs []chatMessage, tools []chatTool, t *turn) (chatMessage, ports.TurnResult, error) {
	body := chatRequest{
		Model:             a.cfg.Model,
		Messages:          msgs,
		Tools:             tools,
		ToolChoice:        "auto",
		ParallelToolCalls: false,
		Stream:            true,
		Temperature:       temperature,
		MaxTokens:         maxTokens,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, ports.TurnResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+chatCompletionsPath, bytes.NewReader(raw))
	if err != nil {
		return chatMessage{}, ports.TurnResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return chatMessage{}, ports.TurnResult{}, fmt.Errorf("mistral dialogue: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return chatMessage{}, ports.TurnResult{}, decodeAPIError(resp)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: expected text/event-stream response")
	}
	return a.readChatStream(bufio.NewReader(resp.Body), tools, t)
}

// readChatStream parses the SSE response into streamed text and assembled tool calls.
func (a *Adapter) readChatStream(br *bufio.Reader, tools []chatTool, t *turn) (chatMessage, ports.TurnResult, error) {
	allowed := map[string]bool{reservedToolName: true}
	for _, tool := range tools {
		allowed[tool.Function.Name] = true
	}

	var text strings.Builder
	calls := []assembledCall{}
	finishReason := ""

	for {
		payload, err := readSSEEvent(br, maxSSEEventBytes)
		if errors.Is(err, io.EOF) {
			return chatMessage{}, ports.TurnResult{}, ErrTurnTruncated // EOF before [DONE]/terminal
		}
		if err != nil {
			return chatMessage{}, ports.TurnResult{}, fmt.Errorf("mistral dialogue: sse read: %w", err)
		}
		if string(payload) == "[DONE]" {
			if finishReason == "" {
				return chatMessage{}, ports.TurnResult{}, ErrTurnTruncated
			}
			break
		}

		var chunk chatChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return chatMessage{}, ports.TurnResult{}, fmt.Errorf("mistral dialogue: malformed sse data: %w", err)
		}
		if len(chunk.Choices) > 1 {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: multiple choices not supported")
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		delta, err := decodeContent(choice.Delta.Content)
		if err != nil {
			return chatMessage{}, ports.TurnResult{}, err
		}
		if delta != "" {
			if text.Len()+len(delta) > maxAccumText {
				return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: assistant text exceeds 64 KiB")
			}
			text.WriteString(delta)
			t.emit(delta)
		}

		for _, tc := range choice.Delta.ToolCalls {
			if tc.Index == nil {
				return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: tool call missing index")
			}
			idx := *tc.Index
			if idx < 0 || idx >= maxCalls {
				return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: too many tool calls")
			}
			for len(calls) <= idx {
				calls = append(calls, assembledCall{index: len(calls)})
			}
			c := &calls[idx]
			if tc.ID != "" {
				c.id = tc.ID
			}
			if tc.Type != "" {
				c.typ = tc.Type
			}
			if tc.Function.Name != "" {
				c.name = tc.Function.Name
			}
			c.args.WriteString(tc.Function.Arguments)
			if c.args.Len() > maxToolArgs {
				return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: tool arguments exceed 64 KiB")
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	switch finishReason {
	case "stop", "tool_calls":
		// valid
	case "length", "model_length":
		return chatMessage{}, ports.TurnResult{}, ErrTurnTruncated
	default:
		return chatMessage{}, ports.TurnResult{}, fmt.Errorf("mistral dialogue: unexpected finish_reason %q", finishReason)
	}

	assistant := chatMessage{Role: "assistant", Content: text.String()}
	if len(calls) == 0 {
		return assistant, ports.TurnResult{ToolCalls: []ports.ToolCall{}}, nil
	}
	return finalizeCalls(assistant, text.String(), calls, allowed)
}

// decodeContent decodes delta.content, which may be a string, an array of content parts,
// or null.
func decodeContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("mistral dialogue: malformed content delta")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

// assembledCall accumulates one fragmented tool call.
type assembledCall struct {
	index int
	id    string
	name  string
	typ   string
	args  strings.Builder
}

// finalizeCalls validates and converts assembled calls, building the assistant message's
// tool_calls and the ports result. The sole finish_call is extracted as an Outcome.
func finalizeCalls(assistant chatMessage, text string, calls []assembledCall, allowed map[string]bool) (chatMessage, ports.TurnResult, error) {
	assistant.Content = text
	ids := map[string]bool{}
	toolCalls := make([]ports.ToolCall, 0, len(calls))
	assistant.ToolCalls = make([]chatToolCall, 0, len(calls))
	for i, c := range calls {
		if c.index != i {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: non-contiguous tool call indices")
		}
		if c.id == "" || ids[c.id] {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: missing or duplicate tool call id")
		}
		ids[c.id] = true
		if c.name == "" {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: tool call missing name")
		}
		if c.typ != "" && c.typ != "function" {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: unexpected tool call type")
		}
		if !allowed[c.name] {
			return chatMessage{}, ports.TurnResult{}, fmt.Errorf("mistral dialogue: unknown tool %q", c.name)
		}
		args := c.args.String()
		if !isJSONObject(args) {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: tool arguments must be one JSON object")
		}
		toolCalls = append(toolCalls, ports.ToolCall{ID: c.id, Name: c.name, Arguments: []byte(args)})
		assistant.ToolCalls = append(assistant.ToolCalls, chatToolCall{
			ID:       c.id,
			Type:     "function",
			Function: toolCallFn{Name: c.name, Arguments: args},
		})
	}

	finishIdx := -1
	for i, tc := range toolCalls {
		if tc.Name == reservedToolName {
			if finishIdx != -1 {
				return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: finish_call returned more than once")
			}
			finishIdx = i
		}
	}
	if finishIdx != -1 {
		if len(toolCalls) != 1 {
			return chatMessage{}, ports.TurnResult{}, errors.New("mistral dialogue: finish_call cannot be combined with other calls")
		}
		out, err := decodeFinishCall(toolCalls[0].Arguments)
		if err != nil {
			return chatMessage{}, ports.TurnResult{}, err
		}
		return assistant, ports.TurnResult{ToolCalls: []ports.ToolCall{}, Outcome: &out}, nil
	}
	return assistant, ports.TurnResult{ToolCalls: toolCalls}, nil
}

// isJSONObject reports whether s decodes as exactly one JSON object.
func isJSONObject(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		return false
	}
	if len(v) == 0 || v[0] != '{' {
		return false
	}
	var extra json.RawMessage
	return dec.Decode(&extra) == io.EOF
}
