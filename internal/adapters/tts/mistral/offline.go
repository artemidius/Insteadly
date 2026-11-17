package mistral

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// offlineStream buffers text and performs one REST/SSE synthesis on Close. No network or
// audio occurs before Close; this latency tradeoff is documented, never a silent fallback.
type offlineStream struct {
	adapter *Adapter
	voice   string

	audio chan domain.AudioChunk

	// state is 0 open, 1 finishing, 2 done. Close and Abort elect exactly one finisher;
	// the loser waits on done (bounded by its own ctx) instead of double-closing Audio.
	mu        sync.Mutex
	state     int
	totalText int
	text      strings.Builder
	closeErr  error

	abortCh   chan struct{}
	abortOnce sync.Once
	done      chan struct{}
}

var _ ports.SpeechStream = (*offlineStream)(nil)

func newOfflineStream(a *Adapter, voice string) *offlineStream {
	return &offlineStream{
		adapter: a,
		voice:   voice,
		audio:   make(chan domain.AudioChunk, audioBufferSize),
		abortCh: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// FeedText appends text locally; no request is made until Close.
func (s *offlineStream) FeedText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if !utf8.ValidString(text) {
		return errors.New("mistral tts: invalid UTF-8 text")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != 0 {
		if s.closeErr != nil {
			return s.closeErr
		}
		return ErrStreamClosed
	}
	if s.totalText+len(text) > maxTotalText {
		return errors.New("mistral tts: total text exceeds 64 KiB")
	}
	s.totalText += len(text)
	s.text.WriteString(text)
	return nil
}

// Audio returns the canonical PCM16 channel, closed by Close.
func (s *offlineStream) Audio() <-chan domain.AudioChunk { return s.audio }

// Close sends exactly one POST /v1/audio/speech (stream=true), feeds the SSE deltas through
// the shared conversion pipeline, and closes Audio(). Idempotent. An Abort before the
// request prevents it; an Abort during synthesis cancels it.
func (s *offlineStream) Close(ctx context.Context) error {
	if !s.beginFinish() {
		select {
		case <-s.done:
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.mu.Lock()
	text := s.text.String()
	s.mu.Unlock()

	if strings.TrimSpace(text) == "" {
		s.finishFinish(errors.New("mistral tts: empty synthesis input"))
		return errors.New("mistral tts: empty synthesis input")
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-s.abortCh:
			cancel()
		case <-watchDone:
		}
	}()
	err := s.synthesize(reqCtx, text)
	close(watchDone)
	s.finishFinish(err)
	return err
}

// Abort discards buffered text without a request, or cancels an in-flight synthesis. It
// closes Audio exactly once and is idempotent; safe before, during, or after Close.
func (s *offlineStream) Abort(ctx context.Context) error {
	s.abortOnce.Do(func() { close(s.abortCh) })
	if s.beginFinish() {
		s.finishFinish(nil)
		return nil
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}

// beginFinish elects this caller as the one that closes Audio.
func (s *offlineStream) beginFinish() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != 0 {
		return false
	}
	s.state = 1
	return true
}

func (s *offlineStream) finishFinish(err error) {
	s.mu.Lock()
	s.closeErr = err
	s.state = 2
	s.mu.Unlock()
	close(s.audio)
	close(s.done)
}

func (s *offlineStream) synthesize(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"model":           s.adapter.cfg.Model,
		"input":           text,
		"voice_id":        s.voice,
		"response_format": "pcm",
		"stream":          true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.adapter.baseURL+offlineSpeechPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.adapter.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.adapter.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mistral tts: offline request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	return s.consumeSSE(ctx, resp.Body)
}

// consumeSSE feeds audio deltas through the float32→resample→frame pipeline.
func (s *offlineStream) consumeSSE(ctx context.Context, r io.Reader) error {
	br := bufio.NewReader(r)
	res := newResampler()
	fr := &framer{}
	sawDone := false

	for {
		payload, eventName, err := readSSEEvent(br, maxSSEEventBytes)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("mistral tts: sse read: %w", err)
		}
		if string(payload) == "[DONE]" {
			break
		}
		var ev ttsEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return fmt.Errorf("mistral tts: malformed sse data: %w", err)
		}
		// Cross-check the SSE event name against the authoritative JSON type when present.
		if eventName != "" && ev.Type != "" && eventName != ev.Type {
			return &ProtocolError{Code: "sse event/type mismatch"}
		}
		switch ev.Type {
		case "speech.audio.delta":
			raw, err := base64Decode(ev.AudioData)
			if err != nil {
				return &ProtocolError{Code: "malformed audio delta"}
			}
			samples, err := decodeFloat32LE(raw)
			if err != nil {
				return &ProtocolError{Code: "malformed audio delta"}
			}
			if err := s.emit(ctx, fr.push(int16LE(res.feed(samples)))); err != nil {
				return err
			}
		case "speech.audio.done":
			sawDone = true
		case "error":
			return &ProtocolError{Code: errorCode(ev)}
		default:
			// ignore unknown typed events
		}
	}
	if !sawDone {
		return &ProtocolError{Code: "speech.audio.done missing"}
	}
	// Flush tail and pad a final frame.
	frames := fr.push(int16LE(res.finish()))
	if pad := fr.flush(); pad != nil {
		frames = append(frames, pad)
	}
	return s.emit(ctx, frames)
}

// emit applies backpressure and is interruptible by the request context, so Close honors a
// canceled context even when the audio buffer is full.
func (s *offlineStream) emit(ctx context.Context, frames [][]byte) error {
	for _, f := range frames {
		select {
		case s.audio <- domain.AudioChunk{Data: f}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func errorCode(ev ttsEvent) string {
	if ev.Code != "" {
		return ev.Code
	}
	if ev.Error != nil && ev.Error.Code != "" {
		return ev.Error.Code
	}
	return "provider_error"
}
