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
