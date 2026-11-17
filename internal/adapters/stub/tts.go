package stub

import (
	"context"
	"errors"
	"sync"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// ttsQueue is the buffer for fake audio chunks. Large enough for test workloads that
// feed a sentence before reading; the stub is not meant for unbounded streaming.
const ttsQueue = 256

// TTS is a fake text-to-speech provider: every FeedText yields exactly one chunk of
// silent canonical audio, so callers can assert the 1:1 mapping.
type TTS struct{}

var _ ports.TextToSpeech = (*TTS)(nil)

// NewTTS returns a fake text-to-speech provider.
func NewTTS() *TTS { return &TTS{} }

// Speak opens a fake synthesis session.
func (t *TTS) Speak(ctx context.Context, opts ports.SpeakOptions) (ports.SpeechStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &speechStream{audio: make(chan domain.AudioChunk, ttsQueue), done: make(chan struct{})}, nil
}

type speechStream struct {
	mu      sync.Mutex
	closed  bool
	aborted bool
	audio   chan domain.AudioChunk
	done    chan struct{}
	sendMu  sync.Mutex
}

var _ ports.SpeechStream = (*speechStream)(nil)

// FeedText emits one silent canonical chunk for the text.
func (s *speechStream) FeedText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("stub tts: feed after close")
	}
	s.mu.Unlock()

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	select {
	case s.audio <- domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}:
		return nil
	case <-s.done:
		return errors.New("stub tts: feed after close")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Audio returns the chunk channel, closed by Close or Abort.
func (s *speechStream) Audio() <-chan domain.AudioChunk { return s.audio }

// Abort discards without flushing and closes Audio. Idempotent.
func (s *speechStream) Abort(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.aborted = true
	close(s.done)
	s.mu.Unlock()

	s.sendMu.Lock()
	close(s.audio)
	s.sendMu.Unlock()
	return nil
}

// Close closes the audio channel. Idempotent.
func (s *speechStream) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ctx.Err()
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()

	s.sendMu.Lock()
	close(s.audio)
	s.sendMu.Unlock()
	return ctx.Err()
}
