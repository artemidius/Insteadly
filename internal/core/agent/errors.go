package agent

import (
	"errors"

	"github.com/artemidius/insteadly/internal/core/domain"
)

var (
	// ErrCallEnded is returned when the call ends before a validated dialogue outcome.
	// The runner owns the fallback outcome derived from the terminal call event.
	ErrCallEnded = errors.New("call ended before dialogue outcome")

	// errListenTimeout means no utterance started inside the configured listen window.
	errListenTimeout = errors.New("agent: listen timeout with no speech")
	// errInboundClosed means the telephony adapter closed inbound audio without a
	// terminal call event: a provider-contract failure, not silent success.
	errInboundClosed = errors.New("agent: inbound audio closed without call end")
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

// C4 conversation sentinels. They are internal control flow or operational failures;
// none is ever copied verbatim into an outcome detail or a provider control instruction.
var (
	// errUtteranceOverflow means the bounded audio-to-STT or final-result queue would lose
	// audio/evidence. Dropping silently and presenting a partial transcript is forbidden.
	errUtteranceOverflow = errors.New("agent: utterance queue overflow")
	// errControlClosed means the bounded post-answer control relay closed before the call
	// ended, so DTMF or playback evidence was lost.
	errControlClosed = errors.New("agent: control relay closed unexpectedly")
	// errControlOverflow means the agent's bounded mark/DTMF router would lose evidence.
	errControlOverflow = errors.New("agent: control evidence overflow")
	// errPlaybackAckTimeout means a provider playback mark did not arrive in time. The
	// segment is not treated as confirmed speech.
	errPlaybackAckTimeout = errors.New("agent: playback acknowledgement timeout")
	// errOutcomeRecoveryExhausted means the per-call outcome-recovery budget was spent.
	errOutcomeRecoveryExhausted = errors.New("agent: outcome recovery budget exhausted")
	// errTerminalDuringTurn means a terminal call event won while a turn was in flight.
	errTerminalDuringTurn = errors.New("agent: call ended during assistant turn")
)

// Fixed, non-secret outcome details. Transcripts, menus, model prose, provider bodies,
// validation strings, and patient values never appear here or in logs.
const (
	detailIVRDeadEnd     = "the automated phone menu reached a dead end"
	detailHoldTimeout    = "hold timeout"
	detailRecoveryFailed = "the conversation could not produce a valid outcome"
	detailPlaybackFailed = "the assistant audio could not be played"
)

// sttLanguage is the fixed core streaming language; it is not an env surface.
const sttLanguage = "de-AT"

// ivrDeadEndOutcome returns the direct structured outcome for an unrecoverable menu. The
// runner still owns hangup and persistence.
func ivrDeadEndOutcome() domain.Outcome {
	return domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "ivr_dead_end", Detail: detailIVRDeadEnd}
}

// holdTimeoutOutcome returns failed/other; hold is not an IVR dead end and never a
// fabricated callback.
func holdTimeoutOutcome() domain.Outcome {
	return domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "other", Detail: detailHoldTimeout}
}
