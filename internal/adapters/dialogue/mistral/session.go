package mistral

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// session implements ports.DialogueSession: committed history, one in-flight turn at a
// time, exact tool-result continuation, and terminal finish_call state.
type session struct {
	adapter       *Adapter
	orderID       domain.OrderID
	prompt        string
	tools         []chatTool
	callerTools   []domain.Tool
	outcomeSchema []byte

	mu           sync.Mutex
	messages     []chatMessage
	pending      *turn
	pendingCalls []ports.ToolCall // ordered ordinary tool calls from the last turn
	finished     bool
	closed       bool

	baseCtx    context.Context
	baseCancel context.CancelFunc
}

var _ ports.DialogueSession = (*session)(nil)

// SendUserTurn feeds the next user utterance and returns one assistant turn.
func (s *session) SendUserTurn(ctx context.Context, text string) (ports.AssistantTurn, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("mistral dialogue: empty user turn")
	}
	s.mu.Lock()
	if s.closed || s.finished {
		s.mu.Unlock()
		return nil, ErrSessionFinished
	}
	if s.pending != nil {
		s.mu.Unlock()
		return nil, ErrTurnInProgress
	}
	candidate := append(append([]chatMessage(nil), s.messages...), chatMessage{Role: "user", Content: text})
	t := newTurn()
	s.pending = t
	s.mu.Unlock()

	go s.executeTurn(ctx, t, candidate)
	return t, nil
}

// SubmitToolResults returns execution results for the previous turn's tool calls and
// continues the model.
func (s *session) SubmitToolResults(ctx context.Context, results []ports.ToolResult) (ports.AssistantTurn, error) {
	s.mu.Lock()
	if s.closed || s.finished {
		s.mu.Unlock()
		return nil, ErrSessionFinished
	}
	if s.pending != nil {
		s.mu.Unlock()
		return nil, ErrTurnInProgress
	}
	if len(s.pendingCalls) == 0 {
		s.mu.Unlock()
		return nil, errors.New("mistral dialogue: no pending tool calls")
	}
	toolMsgs, err := buildToolMessages(s.pendingCalls, results)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	candidate := append(append([]chatMessage(nil), s.messages...), toolMsgs...)
	t := newTurn()
	s.pending = t
	s.mu.Unlock()

	go s.executeTurn(ctx, t, candidate)
	return t, nil
}

// Close cancels an active request and is idempotent; it makes no provider request.
func (s *session) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.baseCancel()
	}
	s.mu.Unlock()
	return ctx.Err()
}

// executeTurn runs one Chat Completions request and commits history only on success.
func (s *session) executeTurn(ctx context.Context, t *turn, candidate []chatMessage) {
	reqCtx, reqCancel := context.WithCancel(s.baseCtx)
	defer reqCancel()
	stop := context.AfterFunc(ctx, reqCancel)
	defer stop()

	assistant, res, err := s.adapter.chat(reqCtx, candidate, s.tools, t)

	s.mu.Lock()
	s.pending = nil
	if err == nil {
		s.messages = append(candidate, assistant)
		if res.Outcome != nil {
			s.pendingCalls = nil
			s.finished = true
		} else {
			s.pendingCalls = append([]ports.ToolCall(nil), res.ToolCalls...)
		}
	}
	s.mu.Unlock()

	t.finish(res, err)
}

// buildToolMessages validates results against the pending calls and builds one tool message
// per pending call, in pending-call order.
func buildToolMessages(pending []ports.ToolCall, results []ports.ToolResult) ([]chatMessage, error) {
	if len(results) != len(pending) {
		return nil, errors.New("mistral dialogue: wrong number of tool results")
	}
	byID := make(map[string]ports.ToolResult, len(results))
	for _, r := range results {
		if _, dup := byID[r.ID]; dup {
			return nil, errors.New("mistral dialogue: duplicate tool result id")
		}
		if !json.Valid(r.Result) {
			return nil, errors.New("mistral dialogue: tool result is not valid JSON")
		}
		byID[r.ID] = r
	}

	msgs := make([]chatMessage, 0, len(pending))
	for _, call := range pending {
		r, ok := byID[call.ID]
		if !ok {
			return nil, errors.New("mistral dialogue: missing tool result")
		}
		msgs = append(msgs, chatMessage{
			Role:       "tool",
			Name:       call.Name,
			Content:    string(r.Result),
			ToolCallID: call.ID,
		})
	}
	return msgs, nil
}
