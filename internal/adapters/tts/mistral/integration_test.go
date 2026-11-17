package mistral

import (
	"context"
	"testing"

	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestRealtimeIntegration(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	for _, word := range []string{"Guten ", "Tag, ", "wie ", "kann ", "ich ", "helfen?"} {
		if err := stream.FeedText(context.Background(), word); err != nil {
			t.Fatalf("FeedText(%q): %v", word, err)
		}
	}
	var total int
	done := make(chan struct{})
	go func() {
		for c := range stream.Audio() {
			total += len(c.Data)
		}
		close(done)
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done
	if total == 0 || total%640 != 0 {
		t.Errorf("canonical output = %d bytes, want a positive multiple of 640", total)
	}
}

func TestRealtimeStreamGoroutinesExit(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	for i := 0; i < 5; i++ {
		stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
		if err != nil {
			t.Fatalf("Speak: %v", err)
		}
		rs := stream.(*realtimeStream)
		_ = stream.FeedText(context.Background(), "hi")
		go func() {
			for range stream.Audio() {
			}
		}()
		if err := stream.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case <-rs.writerDone:
		default:
			t.Fatal("writer goroutine did not exit")
		}
		select {
		case <-rs.readerDone:
		default:
			t.Fatal("reader goroutine did not exit")
		}
	}
}

func TestNoRealtimeToOfflineFallback(t *testing.T) {
	// A realtime dial failure must surface as an error, never silently fall back to offline
	// (a retry/fallback could speak text twice).
	a, err := New(Config{
		APIKey:     "k",
		Mode:       "realtime",
		BaseURL:    "http://127.0.0.1:1", // unroutable loopback port
		HTTPClient: newDefaultHTTPClient(),
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Speak(context.Background(), ports.SpeakOptions{}); err == nil {
		t.Error("expected realtime dial error, not silent fallback")
	}
}
