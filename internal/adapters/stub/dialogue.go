package stub

import (
	"context"
	"fmt"
	"strings"

	"github.com/artemidius/insteadly/internal/core/ports"
)

// Dialogue is a fake dialogue provider: it echoes the user turn word by word and never
// produces tool calls or an outcome.
type Dialogue struct{}

var _ ports.Dialogue = (*Dialogue)(nil)

// NewDialogue returns a fake dialogue provider.
func NewDialogue() *Dialogue { return &Dialogue{} }

// NewSession starts a fake conversation for the request.
func (d *Dialogue) NewSession(ctx context.Context, req ports.SessionRequest) (ports.DialogueSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &dialogueSession{}, nil
}

type dialogueSession struct{}

var _ ports.DialogueSession = (*dialogueSession)(nil)

// SendUserTurn returns a turn that echoes the input.
func (s *dialogueSession) SendUserTurn(ctx context.Context, text string) (ports.AssistantTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newTurn("stub: " + text), nil
}

// SubmitToolResults acknowledges the results without changing the conversation state.
func (s *dialogueSession) SubmitToolResults(ctx context.Context, results []ports.ToolResult) (ports.AssistantTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newTurn(fmt.Sprintf("stub: %d tool result(s)", len(results))), nil
}

// Close is a no-op.
func (s *dialogueSession) Close(ctx context.Context) error { return ctx.Err() }

// turn is a pre-filled, already-closed text channel: deterministic and goroutine-free,
// so a consumer that drains TextChunks and then calls Wait can never deadlock.
type turn struct {
	chunks chan string
}

var _ ports.AssistantTurn = (*turn)(nil)

func newTurn(text string) *turn {
	words := strings.Fields(text)
	chunks := make(chan string, len(words))
	for _, w := range words {
		chunks <- w
	}
	close(chunks)
	return &turn{chunks: chunks}
}

// TextChunks returns the already-closed word channel.
func (t *turn) TextChunks() <-chan string { return t.chunks }

// Wait returns an empty result: the fake never calls tools and never finalizes an
// outcome.
func (t *turn) Wait(ctx context.Context) (ports.TurnResult, error) {
	return ports.TurnResult{}, ctx.Err()
}
