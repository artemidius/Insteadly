package domain

import "time"

// CallEventKind is a normalized call progress event. Providers expose their own event
// names; adapters translate them into these values before the core sees them.
type CallEventKind string

const (
	EventDialing      CallEventKind = "dialing"
	EventRinging      CallEventKind = "ringing"
	EventAnswered     CallEventKind = "answered"
	EventDTMF         CallEventKind = "dtmf"
	EventPlaybackMark CallEventKind = "playback_mark"
	EventEnded        CallEventKind = "ended"
	EventError        CallEventKind = "error"
)

// EndReason normalizes provider hangup causes so the core never sees provider codes.
type EndReason string

const (
	EndCompleted  EndReason = "completed" // normal end after a result
	EndBusy       EndReason = "busy"
	EndNoAnswer   EndReason = "no_answer"
	EndRejected   EndReason = "rejected"
	EndIVRDeadEnd EndReason = "ivr_dead_end" // walked an IVR that went nowhere
	EndAborted    EndReason = "aborted"      // user/system cancelled
	EndError      EndReason = "error"
)

// CallEvent is one normalized step in a call's life.
type CallEvent struct {
	CallID    CallID        `json:"call_id"`
	Kind      CallEventKind `json:"kind"`
	DTMF      string        `json:"dtmf,omitempty"`
	EndReason EndReason     `json:"end_reason,omitempty"`
	At        time.Time     `json:"at"`

	// PlaybackID is set only for EventPlaybackMark. It echoes the opaque ID supplied to
	// CallSession.MarkPlayback after all outbound audio queued before that mark has been
	// played. It is a bounded opaque core-generated string, never text, patient data, a
	// provider SID, or a secret.
	PlaybackID string `json:"playback_id,omitempty"`
}

// Transcript is the product of batch (post-call) transcription.
type Transcript struct {
	Text     string
	Segments []TranscriptSegment // empty unless timestamps/diarization requested
}

// TranscriptSegment is one timed, optionally speaker-attributed line.
type TranscriptSegment struct {
	Start   time.Duration
	End     time.Duration
	Text    string
	Speaker string
}
