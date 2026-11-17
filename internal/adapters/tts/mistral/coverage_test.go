package mistral

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestTTSErrorTypes(t *testing.T) {
	e := &APIError{StatusCode: http.StatusBadRequest, Type: "t", Code: "c", Param: "p", RetryAfter: time.Second}
	if e.Error() == "" {
		t.Error("empty APIError string")
	}
	if e.Temporary() {
		t.Error("400 must not be temporary")
	}
	if e.RetryDelay() != time.Second {
		t.Errorf("retry delay = %v", e.RetryDelay())
	}
	if !(&APIError{StatusCode: http.StatusInternalServerError}).Temporary() {
		t.Error("500 must be temporary")
	}
	if !(&APIError{StatusCode: http.StatusTooManyRequests}).Temporary() {
		t.Error("429 must be temporary")
	}
	if (&ProtocolError{Code: "x"}).Error() == "" {
		t.Error("empty ProtocolError string")
	}
}

func TestTTSCodeAndRetryParsing(t *testing.T) {
	if normalizeCode(json.RawMessage(`"abc"`)) != "abc" {
		t.Error("string code not normalized")
	}
	if normalizeCode(json.RawMessage(`42`)) != "42" {
		t.Error("number code not normalized")
	}
	if normalizeCode(json.RawMessage(`null`)) != "" {
		t.Error("null code should be empty")
	}
	if normalizeCode(json.RawMessage(`{}`)) != "{}" {
		t.Error("unparseable code should be returned verbatim")
	}
	if parseRetryAfter("") != 0 {
		t.Error("empty retry-after should be 0")
	}
	if parseRetryAfter("5") != 5*time.Second {
		t.Error("integer retry-after not parsed")
	}
	if parseRetryAfter("nope") != 0 {
		t.Error("unparseable retry-after should be 0")
	}
	if parseRetryAfter("-1") != 0 {
		t.Error("negative retry-after should be 0")
	}
}

func TestTTSAdapterNewValidation(t *testing.T) {
	if _, err := New(Config{APIKey: ""}); err == nil {
		t.Error("empty key must error")
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "http://api.mistral.ai"}); err == nil {
		t.Error("non-loopback http must error")
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "ftp://x"}); err == nil {
		t.Error("ftp scheme must error")
	}
	if _, err := New(Config{APIKey: "k", Mode: "hybrid"}); err == nil {
		t.Error("invalid mode must error")
	}
	if _, err := New(Config{APIKey: "k", BaseURL: "http://localhost:1", Mode: "offline"}); err != nil {
		t.Errorf("valid loopback offline should pass: %v", err)
	}
}

func TestOfflineFeedTextValidation(t *testing.T) {
	a := newOfflineAdapter(t, "http://127.0.0.1:1")
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if err := stream.FeedText(context.Background(), string([]byte{0xff, 0xfe})); err == nil {
		t.Error("invalid UTF-8 must error")
	}
	big := make([]byte, maxTextDelta+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := stream.FeedText(context.Background(), string(big)); err == nil {
		t.Error("oversized delta must error")
	}
}

func TestOfflineSSEErrorEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"code\":\"model_overloaded\"}\n\n"))
	}))
	t.Cleanup(ts.Close)
	a := newOfflineAdapter(t, ts.URL)
	stream, _ := a.Speak(context.Background(), ports.SpeakOptions{})
	_ = stream.FeedText(context.Background(), "hi")
	err := stream.Close(context.Background())
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "model_overloaded" {
		t.Errorf("err = %v, want ProtocolError model_overloaded", err)
	}
	for range stream.Audio() {
	}
}

func TestRealtimeFeedTextTotalCap(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	t.Cleanup(func() {
		go func() {
			for range stream.Audio() {
			}
		}()
		_ = stream.Close(context.Background())
	})

	chunk := make([]byte, 1024)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for i := 0; i < maxTotalText/len(chunk); i++ {
		if err := stream.FeedText(context.Background(), string(chunk)); err != nil {
			t.Fatalf("FeedText: %v", err)
		}
	}
	if err := stream.FeedText(context.Background(), "x"); !errors.Is(err, ErrStreamClosed) && err == nil {
		t.Fatal("expected total-cap error")
	}
}
