package mistral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	resultsBuffer   = 64  // partial transcript events
	audioQueueSize  = 100 // ~2 s of canonical audio at normal framing
	finalDrainLimit = 15 * time.Second
)

// wsEvent is the generic shape of a Mistral realtime STT WebSocket message. Only the
// fields C2 consumes are declared; unknown types/fields are ignored.
type wsEvent struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Code    string          `json:"code"`
	Error   *wsErrorPayload `json:"error"`
	Session json.RawMessage `json:"session"`
}

type wsErrorPayload struct {
	Code string `json:"code"`
}

// transcriptionStream implements ports.TranscriptionStream for one realtime session.
type transcriptionStream struct {
	adapter *Adapter
	conn    *websocket.Conn
	log     *slog.Logger

	results chan ports.TranscriptionEvent
	audioQ  chan []byte

	// endCh signals the writer goroutine to drain queued audio and send input_audio.end.
	endCh      chan struct{}
	writerDone chan struct{}
	readerDone chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	closeErr   error
	feedClosed bool

	closeOnce   sync.Once
	closeResult error

	dropped atomic.Int64
}

var _ ports.TranscriptionStream = (*transcriptionStream)(nil)

func (a *Adapter) transcribeStream(ctx context.Context, opts ports.StreamOptions) (ports.TranscriptionStream, error) {
	lang, err := normalizeLanguage(opts.Language)
	if err != nil {
		return nil, err
	}
	hints := make([]string, 0, len(opts.Hints))
	for _, h := range opts.Hints {
		if strings.TrimSpace(h) == "" {
			return nil, errors.New("mistral stt: empty context bias hint")
		}
		hints = append(hints, h)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		return nil, fmt.Errorf("mistral stt: dial: %w", err)
	}
	conn.SetReadLimit(maxSTTWebSocketMessage)

	if err := a.handshake(dctx, conn, lang, hints); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	s := &transcriptionStream{
		adapter:    a,
		conn:       conn,
		log:        a.log,
		results:    make(chan ports.TranscriptionEvent, resultsBuffer),
		audioQ:     make(chan []byte, audioQueueSize),
		endCh:      make(chan struct{}),
		writerDone: make(chan struct{}),
		readerDone: make(chan struct{}),
		ctx:        sessCtx,
		cancel:     cancel,
	}
	go s.writer()
	go s.reader()
	return s, nil
}

// handshake performs created → update → updated before audio or text is accepted.
func (a *Adapter) handshake(ctx context.Context, conn *websocket.Conn, lang string, hints []string) error {
	var ev wsEvent
	if err := readWS(ctx, conn, &ev); err != nil {
		return fmt.Errorf("mistral stt: session created: %w", err)
	}
	if ev.Type != "session.created" {
		return &ProtocolError{Code: "expected session.created"}
	}

	session := map[string]any{
		"audio_format": map[string]any{
			"encoding":    "pcm_s16le",
			"sample_rate": 16000,
		},
		"target_streaming_delay_ms": a.cfg.StreamingDelayMS,
	}
	if lang != "" {
		session["language"] = lang
	}
	if len(hints) > 0 {
		session["context_bias"] = hints
	}
	if err := writeWS(ctx, conn, map[string]any{"type": "session.update", "session": session}); err != nil {
		return fmt.Errorf("mistral stt: session update: %w", err)
	}

	if err := readWS(ctx, conn, &ev); err != nil {
		return fmt.Errorf("mistral stt: session updated: %w", err)
	}
	if ev.Type != "session.updated" {
		return &ProtocolError{Code: "expected session.updated"}
	}
	if err := verifyUpdatedSession(ev.Session); err != nil {
		return err
	}
	return nil
}

func verifyUpdatedSession(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var s struct {
		AudioFormat *struct {
			Encoding   string `json:"encoding"`
			SampleRate int    `json:"sample_rate"`
		} `json:"audio_format"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if s.AudioFormat == nil {
		return nil
	}
	if s.AudioFormat.Encoding != "" && s.AudioFormat.Encoding != "pcm_s16le" {
		return &ProtocolError{Code: "unexpected audio encoding"}
	}
	if s.AudioFormat.SampleRate != 0 && s.AudioFormat.SampleRate != 16000 {
		return &ProtocolError{Code: "unexpected audio sample rate"}
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

// writer sends input_audio.append messages in queue order. On endCh it drains remaining
// audio then sends input_audio.end exactly once.
func (s *transcriptionStream) writer() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.endCh:
			s.drainAndEnd()
			return
		case data := <-s.audioQ:
			if !s.sendAppend(data) {
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *transcriptionStream) drainAndEnd() {
	for {
		select {
		case data := <-s.audioQ:
			if !s.sendAppend(data) {
				return
			}
		default:
			if err := writeWS(s.ctx, s.conn, map[string]any{"type": "input_audio.end"}); err != nil {
				s.setCloseErr(fmt.Errorf("mistral stt: write input_audio.end: %w", err))
			}
			return
		}
	}
}

func (s *transcriptionStream) sendAppend(data []byte) bool {
	msg := map[string]any{
		"type":  "input_audio.append",
		"audio": base64.StdEncoding.EncodeToString(data),
	}
	if err := writeWS(s.ctx, s.conn, msg); err != nil {
		s.setCloseErr(fmt.Errorf("mistral stt: write audio: %w", err))
		return false
	}
	return true
}

// reader consumes events, emits cumulative partials and exactly one final, and is the only
// goroutine that closes Results().
func (s *transcriptionStream) reader() {
	defer func() {
		close(s.results)
		close(s.readerDone)
	}()

	var accum strings.Builder
	sawDone := false

	for {
		var ev wsEvent
		err := readWS(s.ctx, s.conn, &ev)
		if err != nil {
			if s.ctx.Err() != nil {
				// Session cancelled (Close/CloseNow); nothing more to read.
				return
			}
			if sawDone {
				// The final transcript was delivered; the server may close without a
				// clean 1000 close frame, which is still a successful end.
				return
			}
			code := websocket.CloseStatus(err)
			if code == websocket.StatusNormalClosure {
				s.setCloseErr(&ProtocolError{Code: "final transcript missing"})
				return
			}
			s.setCloseErr(&ProtocolError{Code: fmt.Sprintf("websocket closed (%s)", closeCodeString(code))})
			return
		}

		switch ev.Type {
		case "transcription.text.delta":
			accum.WriteString(ev.Text)
			if ev.Text != "" {
				s.emit(ports.TranscriptionEvent{Text: accum.String(), Final: false, At: time.Now()})
			}
		case "transcription.done":
			sawDone = true
			s.emitFinal(ev.Text)
		case "session.created", "session.updated":
			// Handshake only; never transcript events.
		case "error":
			code := ev.Code
			if ev.Error != nil && ev.Error.Code != "" {
				code = ev.Error.Code
			}
			if code == "" {
				code = "provider_error"
			}
			s.setCloseErr(&ProtocolError{Code: code})
			_ = s.conn.CloseNow()
		default:
			s.log.Debug("mistral stt: unknown event", "event_type", ev.Type)
		}
	}
}

func closeCodeString(code websocket.StatusCode) string {
	if code < 0 {
		return "read_error"
	}
	return fmt.Sprintf("%d", code)
}

// emit enqueues a partial event, dropping the oldest partial (never a final) if the
// consumer is slow.
func (s *transcriptionStream) emit(ev ports.TranscriptionEvent) {
	select {
	case s.results <- ev:
	default:
		select {
		case <-s.results:
		default:
		}
		select {
		case s.results <- ev:
		default:
		}
	}
}

// emitFinal delivers the final event without dropping it and without blocking forever.
func (s *transcriptionStream) emitFinal(text string) {
	ev := ports.TranscriptionEvent{Text: text, Final: true, At: time.Now()}
	for {
		select {
		case s.results <- ev:
			return
		default:
			select {
			case <-s.results: // drop an old partial to make room
			default:
				// Empty but send raced with a consumer; retry once.
			}
		}
	}
}

func (s *transcriptionStream) setCloseErr(err error) {
	s.mu.Lock()
	if s.closeErr == nil {
		s.closeErr = err
	}
	s.mu.Unlock()
}

// Feed accepts canonical PCM16 audio. It rejects empty/odd chunks and returns ErrStreamClosed
// (or a stored failure) after Close or a terminal provider failure. The accept-and-enqueue
// decision is atomic with Close so a Feed racing Close either enqueues before input_audio.end
// is sent or returns an error — never enqueues-after-end.
func (s *transcriptionStream) Feed(ctx context.Context, chunk domain.AudioChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunk.Data) == 0 {
		return errors.New("mistral stt: empty audio chunk")
	}
	if len(chunk.Data)%2 != 0 {
		return errors.New("mistral stt: odd-length audio chunk (not PCM16)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.feedClosed {
		return ErrStreamClosed
	}

	data := bytesClone(chunk.Data)
	// Bounded queue with drop-oldest backpressure (non-blocking).
	select {
	case s.audioQ <- data:
		return nil
	default:
	}
	select {
	case <-s.audioQ:
		s.dropped.Add(1)
	default:
	}
	s.audioQ <- data
	return nil
}

// Results returns the transcript event channel. It is closed by the reader goroutine.
func (s *transcriptionStream) Results() <-chan ports.TranscriptionEvent { return s.results }

// Close ends input and waits for the final transcript. It is idempotent; repeated and
// concurrent calls share one close operation and its stored result.
func (s *transcriptionStream) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.feedClosed = true
		s.mu.Unlock()
		close(s.endCh)

		// Let already-queued audio drain and input_audio.end be sent.
		select {
		case <-s.writerDone:
		case <-ctx.Done():
			_ = s.conn.CloseNow()
		}

		var err error
		s.mu.Lock()
		if s.closeErr != nil {
			err = s.closeErr // early write/provider failure
		}
		s.mu.Unlock()

		if err == nil {
			// Wait for transcription.done and a normal close (bounded final drain).
			timer := time.NewTimer(finalDrainLimit)
			defer timer.Stop()
			select {
			case <-s.readerDone:
			case <-ctx.Done():
				err = ctx.Err()
				_ = s.conn.CloseNow()
			case <-timer.C:
				err = context.DeadlineExceeded
				_ = s.conn.CloseNow()
			}
		} else {
			_ = s.conn.CloseNow()
		}

		// Ensure the reader has fully exited (it will on conn close).
		<-s.readerDone
		s.cancel()

		if dropped := s.dropped.Load(); dropped > 0 {
			s.log.Warn("mistral stt: dropped audio chunks; transcript may be incomplete", "dropped_chunks", dropped)
		}

		if err == nil {
			s.mu.Lock()
			err = s.closeErr
			s.mu.Unlock()
		}
		s.closeResult = err
	})
	return s.closeResult
}

func bytesClone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
