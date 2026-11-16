package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Dialogue is the booking brain: conversation + tools -> streamed replies, tool
// calls, and the structured outcome (DESIGN.md §2.1.3). Providers receive only
// text and tool definitions — no audio, no provider knowledge leaks in.
type Dialogue interface {
	// NewSession starts a booking conversation for one order. OutcomeSchema is
	// the JSON Schema the final domain.Outcome must satisfy; the adapter may use
	// it for constrained output.
	NewSession(ctx context.Context, req SessionRequest) (DialogueSession, error)
}

// SessionRequest is everything the dialogue provider needs to start a session.
type SessionRequest struct {
	OrderID       domain.OrderID
	SystemPrompt  string // optional override; "" = built-in booking prompt
	Tools         []domain.Tool
	OutcomeSchema []byte // JSON Schema
}

// DialogueSession is one booking conversation.
type DialogueSession interface {
	// SendUserTurn feeds the next user utterance and returns one assistant turn.
	SendUserTurn(ctx context.Context, text string) (AssistantTurn, error)

	// SubmitToolResults returns tool execution results for tool calls made during
	// the previous turn; the model continues from them and produces a new turn.
	SubmitToolResults(ctx context.Context, results []ToolResult) (AssistantTurn, error)

	// Close releases provider resources. Idempotent.
	Close(ctx context.Context) error
}

// AssistantTurn represents one assistant reply. TextChunks streams partial text;
// Wait blocks until the turn is complete and reports tool calls + final outcome.
// Consumers MUST drain TextChunks until close before calling Wait.
type AssistantTurn interface {
	// TextChunks streams the assistant text. Closed when the turn is complete.
	TextChunks() <-chan string
	// Wait blocks until the turn is complete and returns its tool calls + outcome.
	Wait(ctx context.Context) (TurnResult, error)
}

// TurnResult is the completed assistant turn.
type TurnResult struct {
	ToolCalls []ToolCall
	Outcome   *domain.Outcome // non-nil when the model finalized the booking outcome
}

// ToolCall is one tool invocation requested by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments []byte // JSON object
}

// ToolResult is the outcome of executing a ToolCall, fed back to the model.
type ToolResult struct {
	ID     string
	Result []byte // JSON, tool-defined shape
}
