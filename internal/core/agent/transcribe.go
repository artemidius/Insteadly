package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// transcribe runs one STT stream for one delimited utterance: open, drain Results
// concurrently, replay frames at the canonical pace, Close as end-of-input, and require
// exactly one final transcript.
func (a *Agent) transcribe(ctx context.Context, frames [][]byte, hints []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stream, err := a.cfg.STT.TranscribeStream(ctx, ports.StreamOptions{
		Language: "de-AT",
		Hints:    hints,
	})
	if err != nil {
		return "", fmt.Errorf("stt open: %w", err)
	}

	type results struct {
		finals   []string
		partials int
	}
	resultsCh := make(chan results, 1)
	go func() {
		var r results
		for event := range stream.Results() {
			if event.Final {
				r.finals = append(r.finals, event.Text)
			} else {
				r.partials++
			}
		}
		resultsCh <- r
	}()

	var feedErr error
	for i, frame := range frames {
		if err := ctx.Err(); err != nil {
			feedErr = err
			break
		}
		if err := stream.Feed(ctx, domain.AudioChunk{Data: frame}); err != nil {
			feedErr = fmt.Errorf("stt feed: %w", err)
			break
		}
		if i < len(frames)-1 {
			if err := a.cfg.Sleep(ctx, a.cfg.ReplayPace); err != nil {
				feedErr = err
				break
			}
		}
	}

	// Close is mandatory: it is the STT end-of-input signal. Use a fresh bounded context
	// so cancellation cannot skip the flush.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.CleanupTimeout)
	defer cancel()
	closeErr := stream.Close(cleanupCtx)

	var res results
	select {
	case res = <-resultsCh:
	case <-cleanupCtx.Done():
		res = results{}
	}

	if feedErr != nil {
		return "", errors.Join(feedErr, closeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("stt close: %w", closeErr)
	}
	if len(res.finals) == 0 {
		return "", errors.New("stt: results closed without a final transcript")
	}
	if len(res.finals) > 1 {
		return "", fmt.Errorf("stt: multiple final transcripts (%d)", len(res.finals))
	}
	return res.finals[0], nil
}
