package mistral

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// realtimeCapture records the fake server's view of the handshake and audio stream.
type realtimeCapture struct {
	mu        sync.Mutex
	auth      string
	path      string
	model     string
	update    map[string]any
	audioData [][]byte
	endSeen   bool
}

func (c *realtimeCapture) get() (auth, path, model string, update map[string]any, audio [][]byte, end bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.path, c.model, c.update, c.audioData, c.endSeen
}

func newRealtimeServer(t *testing.T, script func(t *testing.T, conn *websocket.Conn, c *realtimeCapture)) (*Adapter, *realtimeCapture) {
	t.Helper()
	c := &realtimeCapture{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.auth = r.Header.Get("Authorization")
		c.path = r.URL.Path
		c.model = r.URL.Query().Get("model")
		c.mu.Unlock()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		script(t, conn, c)
	}))
	t.Cleanup(ts.Close)
	return newTestAdapter(t, ts.URL), c
}

func wsWrite(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, v); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func wsReadMap(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var m map[string]any
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return m
}

// defaultScript performs the full handshake, collects audio until input_audio.end, then
// emits two deltas and one final and closes normally.
func defaultScript(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
	wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{"model": "m"}})
	update := wsReadMap(t, conn)
	c.mu.Lock()
	c.update = update
	c.mu.Unlock()
	wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})

	for {
		m := wsReadMap(t, conn)
		switch m["type"] {
		case "input_audio.append":
			raw, _ := base64.StdEncoding.DecodeString(m["audio"].(string))
			c.mu.Lock()
			c.audioData = append(c.audioData, raw)
			c.mu.Unlock()
		case "input_audio.end":
			c.mu.Lock()
			c.endSeen = true
			c.mu.Unlock()
			wsWrite(t, conn, map[string]any{"type": "transcription.text.delta", "text": "Guten"})
			wsWrite(t, conn, map[string]any{"type": "transcription.text.delta", "text": " Tag."})
			wsWrite(t, conn, map[string]any{"type": "transcription.done", "text": "Guten Tag.", "segments": []any{}, "language": "de"})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func TestRealtimeHandshakeAndAuth(t *testing.T) {
	a, c := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{
		Language: "de-AT",
		Hints:    []string{"Ordination Dr. Muster", "Hietzing"},
	})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close(context.Background()) })

	auth, path, model, update, _, _ := c.get()
	if auth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q", auth)
	}
	if path != realtimeTranscriptPath {
		t.Errorf("path = %q", path)
	}
	if model != defaultRealtimeModel {
		t.Errorf("model = %q, want %q", model, defaultRealtimeModel)
	}
	sess := update["session"].(map[string]any)
	af := sess["audio_format"].(map[string]any)
	if af["encoding"] != "pcm_s16le" || af["sample_rate"] != float64(16000) {
		t.Errorf("audio_format = %v", af)
	}
	if sess["target_streaming_delay_ms"] != float64(480) {
		t.Errorf("delay = %v", sess["target_streaming_delay_ms"])
	}
	if sess["language"] != "de" {
		t.Errorf("language = %v", sess["language"])
	}
	bias := sess["context_bias"].([]any)
	if len(bias) != 2 || bias[0] != "Ordination Dr. Muster" {
		t.Errorf("context_bias = %v", bias)
	}
	if _, ok := sess["turn_detection"]; ok {
		t.Error("turn_detection must not be sent")
	}
}

func TestRealtimeFullFlow(t *testing.T) {
	a, c := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}

	chunks := [][]byte{
		make([]byte, 640),
		make([]byte, 640),
	}
	for i := range chunks {
		for j := range chunks[i] {
			chunks[i][j] = byte(i*100 + j)
		}
	}
	for _, ch := range chunks {
		if err := stream.Feed(context.Background(), domain.AudioChunk{Data: ch}); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}

	// Drain results while Close is called, so the final event is consumed.
	resultsDone := make(chan []ports.TranscriptionEvent, 1)
	go func() {
		var events []ports.TranscriptionEvent
		for ev := range stream.Results() {
			events = append(events, ev)
		}
		resultsDone <- events
	}()

	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := <-resultsDone

	// Partials are cumulative: Guten, then Guten Tag.
	if len(events) < 3 {
		t.Fatalf("events = %d, want at least 3", len(events))
	}
	if events[0].Text != "Guten" || events[0].Final {
		t.Errorf("first partial = %+v", events[0])
	}
	if events[1].Text != "Guten Tag." || events[1].Final {
		t.Errorf("second partial = %+v", events[1])
	}
	last := events[len(events)-1]
	if last.Text != "Guten Tag." || !last.Final {
		t.Errorf("final = %+v", last)
	}

	_, _, _, _, audio, end := c.get()
	if !end {
		t.Error("input_audio.end not sent")
	}
	if len(audio) != len(chunks) {
		t.Fatalf("audio chunks = %d, want %d", len(audio), len(chunks))
	}
	for i := range chunks {
		if string(audio[i]) != string(chunks[i]) {
			t.Errorf("audio chunk %d not byte-identical", i)
		}
	}
}

func TestRealtimeEmptyFinal(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // first append
		_ = wsReadMap(t, conn) // end
		wsWrite(t, conn, map[string]any{"type": "transcription.done", "text": ""})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, 640)}); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	done := make(chan []ports.TranscriptionEvent, 1)
	go func() {
		var evs []ports.TranscriptionEvent
		for ev := range stream.Results() {
			evs = append(evs, ev)
		}
		done <- evs
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	evs := <-done
	if len(evs) != 1 || !evs[0].Final || evs[0].Text != "" {
		t.Errorf("events = %+v, want exactly one empty final", evs)
	}
}

func TestRealtimeFeedValidation(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close(context.Background()) })
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: nil}); err == nil {
		t.Error("empty chunk must error")
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, 3)}); err == nil {
		t.Error("odd chunk must error")
	}
}

func TestRealtimeFeedAfterClose(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	go func() {
		for range stream.Results() {
		}
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, 640)}); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("Feed after close = %v, want ErrStreamClosed", err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestRealtimeProviderError(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // first append
		wsWrite(t, conn, map[string]any{"type": "error", "code": "model_overloaded", "message": "sensitive"})
		_ = conn.CloseNow()
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, 640)}); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	go func() {
		for range stream.Results() {
		}
	}()
	err = stream.Close(context.Background())
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "model_overloaded" {
		t.Errorf("Close = %v, want ProtocolError model_overloaded", err)
	}
}

func TestRealtimeCleanCloseBeforeDone(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn)                            // first append
		_ = wsReadMap(t, conn)                            // end
		_ = conn.Close(websocket.StatusNormalClosure, "") // no transcription.done
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, 640)}); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	go func() {
		for range stream.Results() {
		}
	}()
	err = stream.Close(context.Background())
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "final transcript missing" {
		t.Errorf("Close = %v, want final-transcript-missing protocol error", err)
	}
}

func TestRealtimeHandshakeRejectsWrongUpdate(t *testing.T) {
	cases := map[string]func(t *testing.T, conn *websocket.Conn){
		"not created first": func(t *testing.T, conn *websocket.Conn) {
			wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		},
		"wrong encoding": func(t *testing.T, conn *websocket.Conn) {
			wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
			_ = wsReadMap(t, conn)
			wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{"audio_format": map[string]any{"encoding": "mulaw", "sample_rate": 8000}}})
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			a, _ := newRealtimeServer(t, func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
				script(t, conn)
				_ = conn.CloseNow()
			})
			if _, err := a.TranscribeStream(context.Background(), ports.StreamOptions{}); err == nil {
				t.Fatal("expected handshake error")
			}
		})
	}
}

func TestRealtimeNon101Handshake(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"object":"error","type":"auth_error","code":"invalid_api_key","message":"keep-out"}`))
	}))
	t.Cleanup(ts.Close)
	a := newTestAdapter(t, ts.URL)
	_, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "invalid_api_key" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if err.Error() == "" {
		t.Error("empty error string")
	}
}

func TestFeedDropOldest(t *testing.T) {
	// Construct a stream with a tiny queue and no writer to observe drop-oldest directly.
	s := &transcriptionStream{
		audioQ: make(chan []byte, 1),
		ctx:    context.Background(),
		log:    testLogger(),
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		data := []byte{byte(i), byte(i + 10)}
		if err := s.Feed(ctx, domain.AudioChunk{Data: data}); err != nil {
			t.Fatalf("Feed %d: %v", i, err)
		}
	}
	if got := s.dropped.Load(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	// The surviving chunk is the last one fed.
	select {
	case data := <-s.audioQ:
		if string(data) != string([]byte{2, 12}) {
			t.Errorf("surviving chunk = %v, want {2,12}", data)
		}
	default:
		t.Fatal("queue is empty")
	}
}

func TestRealtimeConcurrentFeedAndClose(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				data := make([]byte, 640)
				data[0] = byte(g)
				data[1] = byte(i)
				_ = stream.Feed(context.Background(), domain.AudioChunk{Data: data})
			}
		}(g)
	}
	go func() {
		for range stream.Results() {
		}
	}()
	wg.Wait()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

var _ ports.TranscriptionStream = (*transcriptionStream)(nil)

// doneWithoutCloseScript sends the terminal transcription.done and then deliberately keeps
// the WebSocket open (no close frame), which is what the real provider may do.
func doneWithoutCloseScript(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
	wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
	update := wsReadMap(t, conn)
	c.mu.Lock()
	c.update = update
	c.mu.Unlock()
	wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})

	for {
		m := wsReadMap(t, conn)
		switch m["type"] {
		case "input_audio.append":
			raw, _ := base64.StdEncoding.DecodeString(m["audio"].(string))
			c.mu.Lock()
			c.audioData = append(c.audioData, raw)
			c.mu.Unlock()
		case "input_audio.end":
			c.mu.Lock()
			c.endSeen = true
			c.mu.Unlock()
			wsWrite(t, conn, map[string]any{"type": "transcription.text.delta", "text": "Guten"})
			wsWrite(t, conn, map[string]any{"type": "transcription.done", "text": "Guten Tag.", "segments": []any{}, "language": "de"})
			return // keep the connection open: no close frame
		}
	}
}

func TestRealtimeDoneDoesNotWaitForServerClose(t *testing.T) {
	a, _ := newRealtimeServer(t, doneWithoutCloseScript)
	stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{Language: "de-AT"})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if err := stream.Feed(context.Background(), domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var finals []string
	drained := make(chan struct{})
	go func() {
		for ev := range stream.Results() {
			if ev.Final {
				finals = append(finals, ev.Text)
			}
		}
		close(drained)
	}()

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err = stream.Close(closeCtx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Close: %v (a terminal done must not wait for a server close frame)", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Close took %v; the reader waited for a server close frame", elapsed)
	}
	<-drained
	if len(finals) != 1 || finals[0] != "Guten Tag." {
		t.Fatalf("finals = %q, want one final transcript", finals)
	}
}
