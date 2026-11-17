package mistral

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// TestFeedTextCloseRaceNoLostText feeds text concurrently with Close and asserts every
// FeedText that returned nil reached the provider before input_text.end.
func TestFeedTextCloseRaceNoLostText(t *testing.T) {
	const n = 16
	for iter := 0; iter < 10; iter++ {
		a, c := newRealtimeServer(t, defaultScript)
		stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
		if err != nil {
			t.Fatalf("Speak: %v", err)
		}
		go func() {
			for range stream.Audio() {
			}
		}()

		var accepted atomic.Int64
		var feedWG, closeWG sync.WaitGroup

		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			_ = stream.Close(context.Background())
		}()

		for i := 0; i < n; i++ {
			feedWG.Add(1)
			go func(i int) {
				defer feedWG.Done()
				err := stream.FeedText(context.Background(), "word")
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, ErrStreamClosed):
					// Not accepted; must not reach the provider.
				default:
					t.Errorf("unexpected FeedText error: %v", err)
				}
			}(i)
		}
		feedWG.Wait()
		closeWG.Wait()

		_, _, _, _, texts, _ := c.get()
		if int64(len(texts)) != accepted.Load() {
			t.Fatalf("iteration %d: delivered %d texts, accepted %d (text lost or duplicated)", iter, len(texts), accepted.Load())
		}
	}
}

// TestRealtimeAbnormalCloseAfterAudioDone verifies a non-1000 close after speech.audio.done
// is still a successful end.
func TestRealtimeAbnormalCloseAfterAudioDone(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // append
		_ = wsReadMap(t, conn) // end
		wsWrite(t, conn, map[string]any{"type": "speech.audio.delta", "audio_data": ""})
		wsWrite(t, conn, map[string]any{"type": "speech.audio.done"})
		_ = conn.CloseNow()
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v, want nil after audio.done", err)
	}
}

// TestRealtimeCloseContextDeadline verifies Close honors a deadline context.
func TestRealtimeCloseContextDeadline(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // append
		_ = wsReadMap(t, conn) // end
		<-conn.CloseRead(context.Background()).Done()
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = stream.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context deadline/canceled", err)
	}
}

// TestConcurrentClose ensures repeated concurrent Close calls share one result.
func TestConcurrentClose(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()

	const c = 8
	errs := make(chan error, c)
	var wg sync.WaitGroup
	for i := 0; i < c; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- stream.Close(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close = %v, want nil", err)
		}
	}
}

// TestOfflineEmitInterruptible verifies a blocked offline producer is interruptible by ctx.
func TestOfflineEmitInterruptible(t *testing.T) {
	s := &offlineStream{audio: make(chan domain.AudioChunk, audioBufferSize)}
	for i := 0; i < audioBufferSize; i++ {
		s.audio <- domain.AudioChunk{Data: make([]byte, 640)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.emit(ctx, [][]byte{make([]byte, 640)}); !errors.Is(err, context.Canceled) {
		t.Errorf("emit = %v, want context.Canceled", err)
	}
}
