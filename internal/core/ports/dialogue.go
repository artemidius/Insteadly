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

	// Interrupt reconciles the most recent assistant turn after its turn context was
	// canceled and Wait returned. confirmedText is the exact concatenation of complete
	// assistant segments whose playback marks were observed before ClearAudio; it is not
	// the full generated response and never contains an estimated partial word. The
	// adapter retains the triggering user/tool history, drops unexecuted calls/outcome
	// from the interrupted assistant turn, and records only confirmedText as assistant
	// speech. Empty confirmedText means no assistant text was confirmed. Already executed
	// tool history is never erased. Interrupt reopens a finish_call-completed session
	// when the core interrupted before returning that outcome.
	Interrupt(ctx context.Context, confirmedText string) error

	// SendControl starts one assistant turn from a fixed core-owned control instruction.
	// It is not a transcription and must not be represented as something the far end
	// said. The adapter preserves the candidate user/tool messages of a failed or
	// stalled turn when retrying it. The same streaming AssistantTurn contract and
	// ordinary tool/outcome limits apply. instruction must be non-empty.
	SendControl(ctx context.Context, instruction string) (AssistantTurn, error)

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
