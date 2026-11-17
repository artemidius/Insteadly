package mistral

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/ports"
)

// abortScript performs the handshake, records text, and reports whether input_text.end was
// ever sent. It tolerates the client closing the socket during Abort.
func abortScript(t *testing.T, conn *websocket.Conn, c *ttsCapture, textSeen chan<- string) {
	t.Helper()
	wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
	if _, err := readMap(conn); err != nil {
		return
	}
	wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
	for {
		m, err := readMap(conn)
		if err != nil {
			return
		}
		switch m["type"] {
		case "input_text.append":
			c.mu.Lock()
			c.texts = append(c.texts, m["text"].(string))
			c.mu.Unlock()
			select {
			case textSeen <- m["text"].(string):
			default:
			}
		case "input_text.end":
			c.mu.Lock()
			c.endSeen = true
			c.mu.Unlock()
		}
	}
}

func readMap(conn *websocket.Conn) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var m map[string]any
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func TestRealtimeAbortBeforeTextClosesAudioAndDoesNotFlush(t *testing.T) {
	textSeen := make(chan string, 4)
	a, c := newRealtimeServer(t, func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		abortScript(t, conn, c, textSeen)
	})
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	audioClosed := make(chan struct{})
	go func() {
		for range stream.Audio() {
		}
		close(audioClosed)
	}()
	if err := stream.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	select {
	case <-audioClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not close Audio")
	}
	if err := stream.FeedText(context.Background(), "late"); err == nil {
		t.Error("FeedText after Abort succeeded")
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Errorf("Close after Abort: %v", err)
	}
	// Abort is idempotent and safe after Close.
	if err := stream.Abort(context.Background()); err != nil {
		t.Errorf("second Abort: %v", err)
	}
	_, _, _, _, _, end := c.get()
	if end {
		t.Error("Abort flushed input_text.end")
	}
}

func TestRealtimeAbortDuringGenerationCancelsWithoutFlush(t *testing.T) {
	textSeen := make(chan string, 4)
	a, c := newRealtimeServer(t, func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		abortScript(t, conn, c, textSeen)
	})
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	go func() {
		for range stream.Audio() {
		}
	}()
	if err := stream.FeedText(context.Background(), "Guten Tag."); err != nil {
		t.Fatalf("FeedText: %v", err)
	}
	select {
	case <-textSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive text")
	}
	if err := stream.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// A normal Close after Abort must not send queued text or input_text.end.
	if err := stream.Close(context.Background()); err != nil {
		t.Errorf("Close after Abort: %v", err)
	}
	_, _, _, _, texts, end := c.get()
	if end {
		t.Error("Abort sent input_text.end")
	}
	if len(texts) != 1 {
		t.Errorf("texts = %q, want only the pre-abort feed", texts)
	}
}

func TestOfflineAbortBeforeCloseSendsNoRequest(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if err := stream.FeedText(context.Background(), "Hallo"); err != nil {
		t.Fatalf("FeedText: %v", err)
	}
	audioClosed := make(chan struct{})
	go func() {
		for range stream.Audio() {
		}
		close(audioClosed)
	}()
	if err := stream.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	select {
	case <-audioClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not close Audio")
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Errorf("Close after Abort: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("aborted offline stream made %d HTTP request(s)", calls.Load())
	}
	if err := stream.FeedText(context.Background(), "later"); err == nil {
		t.Error("FeedText after Abort succeeded")
	}
}

func TestOfflineAbortDuringSynthesisCancelsRequest(t *testing.T) {
	var calls atomic.Int32
	requestSeen := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(requestSeen) })
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if err := stream.FeedText(context.Background(), "Hallo"); err != nil {
		t.Fatalf("FeedText: %v", err)
	}
	closed := make(chan error, 1)
	go func() {
		for range stream.Audio() {
		}
		closed <- nil
	}()
	go func() { _ = stream.Close(context.Background()) }()
	select {
	case <-requestSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("synthesis request not started")
	}
	if err := stream.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not cancel the in-flight request")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Audio did not close after Abort")
	}
	if calls.Load() != 1 {
		t.Errorf("requests = %d, want 1", calls.Load())
	}
}
