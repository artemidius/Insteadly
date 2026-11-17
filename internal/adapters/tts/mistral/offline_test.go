package mistral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/artemidius/insteadly/internal/core/ports"
)

func offlineServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Adapter, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return newTestAdapter(t, ts.URL), ts
}

func newOfflineAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{
		APIKey:     testAPIKey,
		BaseURL:    baseURL,
		Mode:       "offline",
		HTTPClient: &http.Client{},
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestOfflineNoRequestBeforeClose(t *testing.T) {
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
	if calls.Load() != 0 {
		t.Fatal("offline Speak/FeedText must not make a request")
	}
}

func TestOfflineCloseRequest(t *testing.T) {
	type gotReq struct {
		body map[string]any
		auth string
		ct   string
		ac   string
	}
	var got gotReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		got.auth = r.Header.Get("Authorization")
		got.ct = r.Header.Get("Content-Type")
		got.ac = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\"}\n\n"))
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)

	stream, err := a.Speak(context.Background(), ports.SpeakOptions{Voice: "voice_9"})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "Guten ")
	_ = stream.FeedText(context.Background(), "Tag.")
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for range stream.Audio() {
	}

	if got.auth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q", got.auth)
	}
	if !strings.HasPrefix(got.ct, "application/json") {
		t.Errorf("content-type = %q", got.ct)
	}
	if got.ac != "text/event-stream" {
		t.Errorf("accept = %q", got.ac)
	}
	if got.body["model"] != defaultModel {
		t.Errorf("model = %v", got.body["model"])
	}
	if got.body["input"] != "Guten Tag." {
		t.Errorf("input = %v", got.body["input"])
	}
	if got.body["voice_id"] != "voice_9" {
		t.Errorf("voice_id = %v", got.body["voice_id"])
	}
	if got.body["response_format"] != "pcm" || got.body["stream"] != true {
		t.Errorf("format/stream = %v / %v", got.body["response_format"], got.body["stream"])
	}
}

func TestOfflineEmptyInput(t *testing.T) {
	a := newOfflineAdapter(t, "http://127.0.0.1:1")
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "   ")
	if err := stream.Close(context.Background()); err == nil {
		t.Fatal("empty/whitespace input must error locally")
	}
	for range stream.Audio() {
	}
}

func TestOfflineSSEAndConversion(t *testing.T) {
	constant := make([]float32, 480)
	for i := range constant {
		constant[i] = 0.5
	}
	b64 := base64.StdEncoding.EncodeToString(float32Bytes(constant))
	body := "event: speech.audio.delta\r\n" +
		`data: {"type":"speech.audio.delta",` + "\r\n" +
		`data: "audio_data":"` + b64 + `"}` + "\r\n" +
		"\r\n" +
		"event: speech.audio.done\r\n" +
		"data: {\"type\":\"speech.audio.done\",\"usage\":{}}\r\n" +
		"\r\n"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)

	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "Hallo")
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var frames [][]byte
	for c := range stream.Audio() {
		frames = append(frames, c.Data)
	}
	if len(frames) != 1 || len(frames[0]) != 640 {
		gotLen := 0
		if len(frames) > 0 {
			gotLen = len(frames[0])
		}
		t.Fatalf("frames = %d (len %d), want one 640-byte frame", len(frames), gotLen)
	}
}

func TestOfflineNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"object":"error","code":"rate_limited"}`)
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	_ = stream.FeedText(context.Background(), "Hallo")
	err := stream.Close(context.Background())
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != 429 || apiErr.Code != "rate_limited" || !apiErr.Temporary() {
		t.Errorf("err = %v, want 429 rate_limited APIError", err)
	}
	for range stream.Audio() {
	}
}

func TestOfflineRepeatedCloseNoDoublePOST(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\"}\n\n"))
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	_ = stream.FeedText(context.Background(), "Hallo")
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for range stream.Audio() {
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("POSTs = %d, want exactly 1", calls.Load())
	}
}

func TestOfflineMalformedSSE(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: not-json\n\n")
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	_ = stream.FeedText(context.Background(), "Hallo")
	if err := stream.Close(context.Background()); err == nil {
		t.Error("malformed SSE data must error")
	}
	for range stream.Audio() {
	}
}

func TestOfflineAudioDoneMissing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	_ = stream.FeedText(context.Background(), "Hallo")
	if err := stream.Close(context.Background()); err == nil {
		t.Error("missing speech.audio.done must error")
	}
	for range stream.Audio() {
	}
}

func TestOfflineConcurrentFeedText(t *testing.T) {
	a := newOfflineAdapter(t, "http://127.0.0.1:1")
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = stream.FeedText(context.Background(), fmt.Sprintf("word%d ", i))
		}(i)
	}
	wg.Wait()
	// Close must fail (empty input rejected is not the point); here it makes a request to
	// an unroutable address, so we only assert no panic/race occurred during concurrent
	// FeedText. Close is skipped to avoid a slow dial.
	_ = stream
}

var _ ports.SpeechStream = (*offlineStream)(nil)
