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
// time, exact tool-result continuation, terminal finish_call state, and conservative
// interruption/control reconciliation.
type session struct {
	adapter *Adapter
	orderID domain.OrderID
	prompt  string
	tools   []chatTool

	mu           sync.Mutex
	messages     []chatMessage
	pending      *turn
	pendingCalls []ports.ToolCall // ordered ordinary tool calls from the last turn
	finished     bool
	closed       bool

	// lastCandidate is the exact message slice the most recent turn requested from the
	// provider (committed history plus the triggering user/tool/control messages). It is
	// retained until the next turn so Interrupt can drop the unconfirmed assistant part
	// and SendControl can retry without inventing user speech. lastFailed marks a turn
	// that never committed, so a later ordinary user turn still keeps the triggering
	// message.
	lastCandidate []chatMessage
	lastFailed    bool

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
	candidate := append(s.baseMessagesLocked(), chatMessage{Role: "user", Content: text})
	t := newTurn()
	s.startTurnLocked(candidate, t)
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
	// The assistant tool call is already committed, so continuation builds on committed
	// history plus the tool messages; it never rolls back executed tool history.
	candidate := append(cloneMessages(s.messages), toolMsgs...)
	t := newTurn()
	s.startTurnLocked(candidate, t)
	s.mu.Unlock()

	go s.executeTurn(ctx, t, candidate)
	return t, nil
}

// SendControl starts one assistant turn from a fixed core-owned control instruction. The
// instruction is a system-role protocol message, never a user transcription. A completed
// finish_call session is reopened so a rejected outcome can be repaired.
func (s *session) SendControl(ctx context.Context, instruction string) (ports.AssistantTurn, error) {
	if strings.TrimSpace(instruction) == "" {
		return nil, errors.New("mistral dialogue: empty control instruction")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionFinished
	}
	if s.pending != nil {
		s.mu.Unlock()
		return nil, ErrTurnInProgress
	}
	candidate := append(s.baseMessagesLocked(), chatMessage{Role: "system", Content: instruction})
	t := newTurn()
	s.finished = false
	s.pendingCalls = nil
	s.startTurnLocked(candidate, t)
	s.mu.Unlock()

	go s.executeTurn(ctx, t, candidate)
	return t, nil
}

// Interrupt reconciles the most recent assistant turn: it keeps the triggering user/tool
// history, drops unexecuted calls/outcome, and records only provider-confirmed assistant
// text. It reopens a finish_call-completed session.
func (s *session) Interrupt(ctx context.Context, confirmedText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionFinished
	}
	if s.pending != nil {
		return ErrTurnInProgress
	}

	base := s.messages
	if s.lastCandidate != nil {
		base = s.lastCandidate
	}
	next := cloneMessages(base)
	if confirmedText != "" {
		next = append(next, chatMessage{Role: "assistant", Content: confirmedText})
	}
	s.messages = next
	s.pendingCalls = nil
	s.finished = false
	s.lastFailed = false
	s.lastCandidate = nil
	return nil
}

// Close cancels an active request and is idempotent; it makes no provider request.
func (s *session) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.lastCandidate = nil
		s.baseCancel()
	}
	s.mu.Unlock()
	return ctx.Err()
}

// startTurnLocked records the candidate and the in-flight turn. Caller holds mu.
func (s *session) startTurnLocked(candidate []chatMessage, t *turn) {
	s.lastCandidate = cloneMessages(candidate)
	s.lastFailed = false
	s.pending = t
}

// baseMessagesLocked returns the history a new turn builds on. A failed or finished last
// turn is rolled back to its triggering candidate so an invalid/stalled assistant turn is
// not treated as committed speech; ordinary successful history is used verbatim.
func (s *session) baseMessagesLocked() []chatMessage {
	if s.lastCandidate != nil && (s.lastFailed || s.finished) {
		return cloneMessages(s.lastCandidate)
	}
	return cloneMessages(s.messages)
}

func cloneMessages(in []chatMessage) []chatMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]chatMessage, len(in))
	for i, m := range in {
		out[i] = m
		if m.ToolCalls != nil {
			out[i].ToolCalls = append([]chatToolCall(nil), m.ToolCalls...)
		}
	}
	return out
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
		s.messages = append(cloneMessages(candidate), assistant)
		if res.Outcome != nil {
			s.pendingCalls = nil
			s.finished = true
		} else {
			s.pendingCalls = append([]ports.ToolCall(nil), res.ToolCalls...)
		}
		s.lastFailed = false
	} else {
		// Keep the candidate and flag it so history reconciliation can retry or drop the
		// unconfirmed assistant part without losing the triggering user/tool messages.
		s.lastFailed = true
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
