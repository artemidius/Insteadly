package mistral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	audioBufferSize = 100 // canonical frames of backpressure
	textQueueSize   = 64  // text deltas
	maxTextDelta    = 64 << 10
	maxTotalText    = 64 << 10
	finalDrainLimit = 30 * time.Second
)

// ttsEvent is the generic shape of a Mistral realtime TTS WebSocket message.
type ttsEvent struct {
	Type      string          `json:"type"`
	AudioData string          `json:"audio_data"`
	Code      string          `json:"code"`
	Error     *wsErrorPayload `json:"error"`
	Session   json.RawMessage `json:"session"`
}

type wsErrorPayload struct {
	Code string `json:"code"`
}

// realtimeStream implements ports.SpeechStream for one realtime generation.
type realtimeStream struct {
	adapter *Adapter
	conn    *websocket.Conn
	log     *slog.Logger

	audio chan domain.AudioChunk
	textQ chan string

	endCh      chan struct{}
	writerDone chan struct{}
	readerDone chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	res *resampler
	fr  *framer

	mu         sync.Mutex
	closeErr   error
	feedClosed bool
	totalText  int

	closeOnce   sync.Once
	closeResult error
}

var _ ports.SpeechStream = (*realtimeStream)(nil)

func (a *Adapter) speakRealtime(ctx context.Context, opts ports.SpeakOptions) (ports.SpeechStream, error) {
	dctx, dcancel := context.WithTimeout(ctx, handshakeTimeout)
	defer dcancel()

	conn, resp, err := websocket.Dial(dctx, a.realtimeURL(), &websocket.DialOptions{
		HTTPClient: a.httpClient,
		HTTPHeader: a.authHeader(),
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, decodeAPIError(resp)
		}
		return nil, fmt.Errorf("mistral tts: dial: %w", err)
	}
	conn.SetReadLimit(maxTTSWebSocketMsg)

	voice := opts.Voice
	if voice == "" {
		voice = a.cfg.DefaultVoice
	}
	if err := a.handshake(dctx, conn, voice); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	s := &realtimeStream{
		adapter:    a,
		conn:       conn,
		log:        a.log,
		audio:      make(chan domain.AudioChunk, audioBufferSize),
		textQ:      make(chan string, textQueueSize),
		endCh:      make(chan struct{}),
		writerDone: make(chan struct{}),
		readerDone: make(chan struct{}),
		ctx:        sessCtx,
		cancel:     cancel,
		res:        newResampler(),
		fr:         &framer{},
	}
	go s.writer()
	go s.reader()
	return s, nil
}

func (a *Adapter) handshake(ctx context.Context, conn *websocket.Conn, voice string) error {
	var ev ttsEvent
	if err := readWS(ctx, conn, &ev); err != nil {
		return fmt.Errorf("mistral tts: session created: %w", err)
	}
	if ev.Type != "session.created" {
		return &ProtocolError{Code: "expected session.created"}
	}
	update := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"voice":           voice,
			"streaming_mode":  "responsive",
			"response_format": "pcm",
		},
	}
	if err := writeWS(ctx, conn, update); err != nil {
		return fmt.Errorf("mistral tts: session update: %w", err)
	}
	if err := readWS(ctx, conn, &ev); err != nil {
		return fmt.Errorf("mistral tts: session updated: %w", err)
	}
	if ev.Type != "session.updated" {
		return &ProtocolError{Code: "expected session.updated"}
	}
	if len(ev.Session) > 0 {
		var s struct {
			ResponseFormat string `json:"response_format"`
		}
		if json.Unmarshal(ev.Session, &s) == nil && s.ResponseFormat != "" && s.ResponseFormat != "pcm" {
			return &ProtocolError{Code: "unexpected response format"}
		}
	}
	return nil
}

func readWS(ctx context.Context, conn *websocket.Conn, v any) error {
	return wsjson.Read(ctx, conn, v)
}

func writeWS(ctx context.Context, conn *websocket.Conn, v any) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(wctx, conn, v)
}

// writer sends input_text.append in order; on endCh it drains then sends input_text.end.
func (s *realtimeStream) writer() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.endCh:
			s.drainAndEnd()
			return
		case text := <-s.textQ:
			if !s.sendText(text) {
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *realtimeStream) drainAndEnd() {
	for {
		select {
		case text := <-s.textQ:
			if !s.sendText(text) {
				return
			}
		default:
			if err := writeWS(s.ctx, s.conn, map[string]any{"type": "input_text.end"}); err != nil {
				s.fail(fmt.Errorf("mistral tts: write input_text.end: %w", err))
			}
			return
		}
	}
}

func (s *realtimeStream) sendText(text string) bool {
	if err := writeWS(s.ctx, s.conn, map[string]any{"type": "input_text.append", "text": text}); err != nil {
		s.fail(fmt.Errorf("mistral tts: write text: %w", err))
		return false
	}
	return true
}

// reader converts audio deltas to canonical frames and is the only goroutine that closes
// Audio().
func (s *realtimeStream) reader() {
	defer func() {
		s.finalFlush()
		close(s.audio)
		close(s.readerDone)
	}()

	audioDone := false
	for {
		var ev ttsEvent
		err := readWS(s.ctx, s.conn, &ev)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			if audioDone {
				// The generation completed; the server may close without a clean 1000
				// close frame, which is still a successful end.
				return
			}
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				s.fail(&ProtocolError{Code: "speech.audio.done missing"})
				return
			}
			s.fail(&ProtocolError{Code: fmt.Sprintf("websocket closed (%d)", websocket.CloseStatus(err))})
			return
		}

		switch ev.Type {
		case "speech.audio.delta":
			raw, err := base64.StdEncoding.DecodeString(ev.AudioData)
			if err != nil {
				s.fail(&ProtocolError{Code: "malformed audio delta"})
				_ = s.conn.CloseNow()
				return
			}
			samples, err := decodeFloat32LE(raw)
			if err != nil {
				s.fail(&ProtocolError{Code: "malformed audio delta"})
				_ = s.conn.CloseNow()
				return
			}
			out := s.res.feed(samples)
			s.emitFrames(s.fr.push(int16LE(out)))
		case "speech.audio.done":
			audioDone = true
		case "session.created", "session.updated":
			// Handshake only.
		case "error":
			code := ev.Code
			if ev.Error != nil && ev.Error.Code != "" {
				code = ev.Error.Code
			}
			if code == "" {
				code = "provider_error"
			}
			s.fail(&ProtocolError{Code: code})
			_ = s.conn.CloseNow()
		default:
			s.log.Debug("mistral tts: unknown event", "event_type", ev.Type)
		}
	}
}

// finalFlush finishes the resampler tail and emits a single padded residual frame if any.
func (s *realtimeStream) finalFlush() {
	out := s.res.finish()
	frames := s.fr.push(int16LE(out))
	if pad := s.fr.flush(); pad != nil {
		frames = append(frames, pad)
	}
	s.emitFrames(frames)
}

// emitFrames applies backpressure (never drops synthesized audio).
func (s *realtimeStream) emitFrames(frames [][]byte) {
	for _, f := range frames {
		select {
		case s.audio <- domain.AudioChunk{Data: f}:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *realtimeStream) fail(err error) {
	s.mu.Lock()
	if s.closeErr == nil {
		s.closeErr = err
	}
	s.mu.Unlock()
	s.cancel()
}

// FeedText enqueues one verbatim text delta (including surrounding spaces). It rejects
// invalid UTF-8 and enforces per-delta and per-session caps.
func (s *realtimeStream) FeedText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if !utf8.ValidString(text) {
		return errors.New("mistral tts: invalid UTF-8 text")
	}
	if len(text) > maxTextDelta {
		return errors.New("mistral tts: text delta exceeds 64 KiB")
	}
	s.mu.Lock()
	if s.feedClosed {
		err := s.closeErr
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrStreamClosed
	}
	if s.totalText+len(text) > maxTotalText {
		s.mu.Unlock()
		return errors.New("mistral tts: total text exceeds 64 KiB")
	}
	s.totalText += len(text)
	s.mu.Unlock()

	select {
	case s.textQ <- text:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrStreamClosed
	}
}

// Audio returns the canonical PCM16 chunk channel, closed by the reader goroutine.
func (s *realtimeStream) Audio() <-chan domain.AudioChunk { return s.audio }

// Close ends input and drains audio through speech.audio.done and the server's normal
// close. Idempotent; concurrent callers share one close operation.
func (s *realtimeStream) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.feedClosed = true
		s.mu.Unlock()
		close(s.endCh)

		select {
		case <-s.writerDone:
		case <-ctx.Done():
			_ = s.conn.CloseNow()
		}

		var err error
		s.mu.Lock()
		if s.closeErr != nil {
			err = s.closeErr
		}
		s.mu.Unlock()

		if err == nil {
			timer := time.NewTimer(finalDrainLimit)
			defer timer.Stop()
			select {
			case <-s.readerDone:
			case <-ctx.Done():
				err = ctx.Err()
				s.cancel()
				_ = s.conn.CloseNow()
			case <-timer.C:
				err = context.DeadlineExceeded
				s.cancel()
				_ = s.conn.CloseNow()
			}
		} else {
			s.cancel()
			_ = s.conn.CloseNow()
		}

		<-s.readerDone
		s.cancel()

		if err == nil {
			s.mu.Lock()
			err = s.closeErr
			s.mu.Unlock()
		}
		s.closeResult = err
	})
	return s.closeResult
}
