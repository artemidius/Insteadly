package agent

import "errors"

var (
	// ErrCallEnded is returned when the call ends before a validated dialogue outcome.
	// The runner owns the fallback outcome derived from the terminal call event.
	ErrCallEnded = errors.New("call ended before dialogue outcome")

	// errListenTimeout means no utterance started inside the configured listen window.
	errListenTimeout = errors.New("agent: listen timeout with no speech")
	// errInboundClosed means the telephony adapter closed inbound audio without a
	// terminal call event: a provider-contract failure, not silent success.
	errInboundClosed = errors.New("agent: inbound audio closed without call end")
	// errEmptyTurn means the assistant produced no text, no tool calls, and no outcome.
	errEmptyTurn = errors.New("agent: empty assistant turn")
	// errEmptyTranscript means a second consecutive utterance produced no final text.
	errEmptyTranscript = errors.New("agent: empty final transcript")
	// errUserTurnsExceeded means the per-call user-turn limit was reached.
	errUserTurnsExceeded = errors.New("agent: maximum user turns exceeded")
	// errToolRoundsExceeded means the consecutive tool-continuation limit was reached.
	errToolRoundsExceeded = errors.New("agent: maximum tool continuations exceeded")
)

// LimitError marks a conversation/inactivity limit rather than a provider or adapter
// failure. The runner maps it to failed/other, as required by the C3 plan §3.4; the
// underlying error stays available through errors.Is/errors.As.
type LimitError struct {
	Err error
}

func (e *LimitError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying limit error.
func (e *LimitError) Unwrap() error { return e.Err }

// IsLimit reports whether err was produced by an agent conversation/inactivity limit
// (listen/silence timeout, repeated empty transcript, turn/tool-round bound, or the
// agent's own maximum call duration). It never matches an arbitrary provider
// context.DeadlineExceeded.
func IsLimit(err error) bool {
	var limitErr *LimitError
	return errors.As(err, &limitErr)
}

// markLimit wraps err once. It is a no-op for nil/already-classified errors.
func markLimit(err error) error {
	if err == nil || IsLimit(err) {
		return err
	}
	return &LimitError{Err: err}
}
