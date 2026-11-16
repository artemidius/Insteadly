package stub

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// STT is a fake speech-to-text provider. Streaming sessions report how many audio
// chunks they consumed; batch mode returns a fixed transcript.
type STT struct{}

var _ ports.SpeechToText = (*STT)(nil)

// NewSTT returns a fake speech-to-text provider.
func NewSTT() *STT { return &STT{} }

// TranscribeStream opens a fake streaming session.
func (s *STT) TranscribeStream(ctx context.Context, opts ports.StreamOptions) (ports.TranscriptionStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &transcriptionStream{results: make(chan ports.TranscriptionEvent, 1)}, nil
}

// TranscribeFile returns a fixed transcript, ignoring the audio.
func (s *STT) TranscribeFile(ctx context.Context, r io.Reader, opts ports.BatchOptions) (domain.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	return domain.Transcript{Text: "stub file transcript"}, nil
}

type transcriptionStream struct {
	mu      sync.Mutex
	chunks  int
	results chan ports.TranscriptionEvent

	closeOnce sync.Once
}

var _ ports.TranscriptionStream = (*transcriptionStream)(nil)

// Feed counts the chunk and never blocks.
func (s *transcriptionStream) Feed(ctx context.Context, chunk domain.AudioChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.chunks++
	s.mu.Unlock()
	return nil
}

// Results returns the event channel, closed by Close after the single final event.
func (s *transcriptionStream) Results() <-chan ports.TranscriptionEvent { return s.results }

// Close emits exactly one final event naming the consumed chunk count, then closes
// the results channel. Idempotent.
func (s *transcriptionStream) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		n := s.chunks
		s.mu.Unlock()
		s.results <- ports.TranscriptionEvent{
			Text:  fmt.Sprintf("stub heard %d chunk(s)", n),
			Final: true,
			At:    time.Now(),
		}
		close(s.results)
	})
	return ctx.Err()
}
