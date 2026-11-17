package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// oneText adapts a fixed string (greeting, repeat prompt) to the streaming speak loop.
func oneText(text string) <-chan string {
	ch := make(chan string, 1)
	ch <- text
	close(ch)
	return ch
}

// speak turns one assistant generation into streamed TTS and outbound call audio.
//
// TTS opens lazily on the first non-empty text delta, so tool-only/outcome-only turns
// create no empty realtime generation. One goroutine drains Audio() into SendAudio; Close
// runs while that drainer is still active, then the drainer is awaited. It reports
// whether any non-empty text was fed.
func (a *Agent) speak(ctx context.Context, call AudioSession, texts <-chan string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	genCtx, genCancel := context.WithCancel(ctx)
	defer genCancel()

	var stream ports.SpeechStream
	var audioDone chan error
	spoke := false

	for {
		select {
		case text, open := <-texts:
			if !open {
				return a.finishSpeech(ctx, stream, audioDone, spoke)
			}
			if text == "" {
				continue
			}
			if stream == nil {
				s, err := a.cfg.TTS.Speak(genCtx, ports.SpeakOptions{})
				if err != nil {
					return false, fmt.Errorf("tts open: %w", err)
				}
				stream = s
				audioDone = make(chan error, 1)
				go a.drainAudio(genCtx, genCancel, call, s.Audio(), audioDone)
			}
			if err := stream.FeedText(genCtx, text); err != nil {
				genCancel()
				cleanupErr := a.closeSpeech(ctx, stream, audioDone)
				return spoke, errors.Join(fmt.Errorf("tts feed: %w", err), cleanupErr)
			}
			spoke = true
		case <-genCtx.Done():
			// Cancellation while feeding: close with a context independent of the
			// canceled generation so the flush is not skipped.
			closeErr := a.closeSpeechOnce(ctx, stream, audioDone)
			if err := ctx.Err(); err != nil {
				return spoke, errors.Join(err, closeErr)
			}
			return spoke, closeErr
		}
	}
}

// finishSpeech implements the normal end-of-turn ordering: TTS Close while the audio
// drainer keeps running, then wait for the drainer to observe channel closure.
func (a *Agent) finishSpeech(ctx context.Context, stream ports.SpeechStream, audioDone <-chan error, spoke bool) (bool, error) {
	if stream == nil {
		return spoke, nil
	}
	return spoke, a.closeSpeech(ctx, stream, audioDone)
}

// closeSpeech flushes TTS (draining its audio meanwhile) and returns the drainer's first
// error. A nil stream is a no-op.
func (a *Agent) closeSpeech(ctx context.Context, stream ports.SpeechStream, audioDone <-chan error) error {
	if stream == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.CleanupTimeout)
	defer cancel()
	closeErr := stream.Close(cleanupCtx)
	var drainErr error
	select {
	case drainErr = <-audioDone:
	case <-cleanupCtx.Done():
		drainErr = cleanupCtx.Err()
	}
	return errors.Join(closeErr, drainErr)
}

// closeSpeechOnce is closeSpeech with a nil guard for the cancellation path where a
// stream may never have opened.
func (a *Agent) closeSpeechOnce(ctx context.Context, stream ports.SpeechStream, audioDone <-chan error) error {
	if stream == nil {
		return nil
	}
	return a.closeSpeech(ctx, stream, audioDone)
}

// drainAudio forwards every canonical TTS chunk to the call. On the first SendAudio
// failure (or cancellation) it cancels the generation but keeps draining until the
// provider closes Audio(), so TTS backpressure can never deadlock Close.
func (a *Agent) drainAudio(ctx context.Context, cancel context.CancelFunc, call AudioSession,
	audio <-chan domain.AudioChunk, done chan<- error) {
	var firstErr error
	for chunk := range audio {
		if firstErr != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			cancel()
			continue
		}
		if len(chunk.Data) == 0 {
			continue
		}
		if err := call.SendAudio(ctx, chunk); err != nil {
			firstErr = fmt.Errorf("send audio: %w", err)
			cancel()
		}
	}
	done <- firstErr
}
