package ports

import (
	"context"
	"io"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// SpeechToText is pure streaming transcription (DESIGN.md §1.6: turn-taking/VAD live
// in the core, not here). Adapters must emit events for canonical audio only.
type SpeechToText interface {
	// TranscribeStream opens a live streaming transcription session.
	TranscribeStream(ctx context.Context, opts StreamOptions) (TranscriptionStream, error)

	// TranscribeFile transcribes a complete recording (post-call). The reader
	// yields canonical audio. Adapters may read it fully before returning.
	TranscribeFile(ctx context.Context, r io.Reader, opts BatchOptions) (domain.Transcript, error)
}

// StreamOptions tunes a live transcription session.
type StreamOptions struct {
	Language string   // BCP-47, "" = provider default
	Hints    []string // context-biasing vocabulary (clinic names, streets); may be ignored
}

// BatchOptions tunes a post-call transcription.
type BatchOptions struct {
	Language    string
	Hints       []string
	Timestamps  bool
	Diarization bool
}

// TranscriptionStream is one live transcription session.
type TranscriptionStream interface {
	// Feed canonical audio. Safe for concurrent use; must not block indefinitely
	// if the provider is slow (bounded internal queue, drop-oldest like Telephony).
	Feed(ctx context.Context, chunk domain.AudioChunk) error

	// Results delivers transcript events (partial then final). Closed when the
	// stream ends. A final event for the last audio is guaranteed only if Close
	// is called; adapters should flush on Close.
	Results() <-chan TranscriptionEvent

	// Close ends the session and releases provider resources. Idempotent.
	Close(ctx context.Context) error
}

// TranscriptionEvent is one partial or final transcription result.
type TranscriptionEvent struct {
	Text  string
	Final bool
	At    time.Time
}

// TextToSpeech turns text chunks into canonical audio chunks, spoken as generated
// (DESIGN.md §2.2: LLM output is spoken while it streams).
type TextToSpeech interface {
	// Speak opens a streaming TTS session.
	Speak(ctx context.Context, opts SpeakOptions) (SpeechStream, error)
}

// SpeakOptions selects the voice for a TTS session.
type SpeakOptions struct {
	Voice string // provider voice id; "" = provider default
}

// SpeechStream is one live synthesis session.
type SpeechStream interface {
	// FeedText enqueues text to synthesize, in order. Audio arrives on Audio()
	// as it is produced (chunking is provider-dependent; the core must not
	// assume 1:1 text-to-audio correspondence).
	FeedText(ctx context.Context, text string) error

	// Audio delivers canonical PCM16 chunks. Closed after the last text fed
	// before Close has been synthesized.
	Audio() <-chan domain.AudioChunk

	// Abort stops generation without flushing pending text. It discards queued/unread
	// synthesis, closes Audio after bounded transport cleanup, and is idempotent. It is
	// safe after normal Close and safe after call cancellation. Abort's context bounds
	// cleanup only; it must not be used to continue synthesis. Audio chunks already
	// returned to the core may still exist and must be discarded by the caller's
	// playback generation.
	Abort(ctx context.Context) error

	// Close flushes and releases provider resources. Idempotent.
	Close(ctx context.Context) error
}
