package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Telephony dials numbers and exposes per-call event + audio streams.
// The core never sees provider concepts (CallSid, streams, µ-law): the adapter
// normalizes everything to domain.CallEvent and canonical audio.
type Telephony interface {
	// Dial initiates an outbound call to order.Clinic.Phone. It returns a live
	// CallSession without waiting for the far end to answer; progress arrives
	// as events. Errors are terminal for this attempt (never partial sessions).
	Dial(ctx context.Context, order domain.Order) (CallSession, error)
}

// CallSession is one live call attempt.
type CallSession interface {
	// ID is the stable identifier for this call attempt.
	ID() domain.CallID

	// Events delivers normalized call progress. The channel is closed when the
	// call ends; the final event is always EventEnded (or EventError followed by
	// EventEnded). Consumers must drain until close.
	Events() <-chan domain.CallEvent

	// Inbound delivers far-end audio in canonical format (PCM16 LE mono 16 kHz).
	// The adapter buffers boundedly (e.g. 100 chunks) and DROPS-OLDEST when the
	// core is slow, counting drops in a log warning — the core must keep up.
	// Closed when the call ends.
	Inbound() <-chan domain.AudioChunk

	// SendAudio enqueues canonical audio for playback in the call. Must be safe
	// for concurrent use and must return (not panic) after the call has ended.
	SendAudio(ctx context.Context, chunk domain.AudioChunk) error

	// SendDTMF sends a digit sequence through the live call (IVR menus).
	SendDTMF(ctx context.Context, digits string) error

	// ClearAudio discards outbound audio queued locally or by the provider but not yet
	// played. It does not close the media stream or end the call. Audio already played
	// cannot be recalled. It is idempotent and ordered against SendAudio/MarkPlayback:
	// audio accepted after a successful clear belongs to the new playback generation.
	// Late marks from an older generation may arrive.
	ClearAudio(ctx context.Context) error

	// MarkPlayback queues an opaque marker after all previously queued outbound audio.
	// The adapter emits exactly one EventPlaybackMark with the same PlaybackID when the
	// provider reports that point was reached. This is playback evidence, not proof that
	// a person heard the audio. IDs are unique within a session. MarkPlayback is ordered
	// with SendAudio and ClearAudio.
	MarkPlayback(ctx context.Context, playbackID string) error

	// Hangup terminates the call. Idempotent; safe after natural end.
	Hangup(ctx context.Context) error
}
