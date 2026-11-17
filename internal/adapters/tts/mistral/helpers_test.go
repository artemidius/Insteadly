package mistral

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const testAPIKey = "test-mistral-api-key"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{
		APIKey:     testAPIKey,
		BaseURL:    baseURL,
		HTTPClient: &http.Client{},
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// ttsCapture records the fake server's view of the realtime handshake and text stream.
type ttsCapture struct {
	mu      sync.Mutex
	auth    string
	path    string
	model   string
	update  map[string]any
	texts   []string
	endSeen bool
}

func (c *ttsCapture) get() (auth, path, model string, update map[string]any, texts []string, end bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.path, c.model, c.update, c.texts, c.endSeen
}

func newRealtimeServer(t *testing.T, script func(t *testing.T, conn *websocket.Conn, c *ttsCapture)) (*Adapter, *ttsCapture) {
	t.Helper()
	c := &ttsCapture{}
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

// defaultScript performs the TTS realtime handshake, collects text until input_text.end,
// sends one split audio delta + done, and closes normally.
func defaultScript(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
	wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
	update := wsReadMap(t, conn)
	c.mu.Lock()
	c.update = update
	c.mu.Unlock()
	wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})

	for {
		m := wsReadMap(t, conn)
		switch m["type"] {
		case "input_text.append":
			c.mu.Lock()
			c.texts = append(c.texts, m["text"].(string))
			c.mu.Unlock()
		case "input_text.end":
			c.mu.Lock()
			c.endSeen = true
			c.mu.Unlock()
			audio := sineFloat32(480, 440)
			half := len(audio) / 2
			wsWrite(t, conn, map[string]any{"type": "speech.audio.delta", "audio_data": base64.StdEncoding.EncodeToString(float32Bytes(audio[:half]))})
			wsWrite(t, conn, map[string]any{"type": "speech.audio.delta", "audio_data": base64.StdEncoding.EncodeToString(float32Bytes(audio[half:]))})
			wsWrite(t, conn, map[string]any{"type": "speech.audio.done"})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

// sineFloat32 builds n float32 samples of a sine at the given frequency (24 kHz).
func sineFloat32(n int, freq float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / ttsInputRate))
	}
	return out
}

func float32Bytes(samples []float32) []byte {
	out := make([]byte, 4*len(samples))
	for i, s := range samples {
		bits := math.Float32bits(s)
		out[4*i] = byte(bits)
		out[4*i+1] = byte(bits >> 8)
		out[4*i+2] = byte(bits >> 16)
		out[4*i+3] = byte(bits >> 24)
	}
	return out
}

func drainAudio(t *testing.T, s interface {
	Audio() <-chan domain.AudioChunk
}) [][]byte {
	t.Helper()
	var out [][]byte
	for c := range s.Audio() {
		out = append(out, c.Data)
	}
	return out
}
