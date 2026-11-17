package mistral

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// TestFeedCloseRaceNoLostAudio feeds concurrently with Close and asserts that every Feed
// which returned nil actually reached the provider before input_audio.end. This guards
// against the split-check-then-enqueue race (audio accepted but never sent).
func TestFeedCloseRaceNoLostAudio(t *testing.T) {
	const n = 16
	for iter := 0; iter < 10; iter++ {
		a, c := newRealtimeServer(t, defaultScript)
		stream, err := a.TranscribeStream(context.Background(), ports.StreamOptions{})
		if err != nil {
			t.Fatalf("TranscribeStream: %v", err)
		}
		go func() {
			for range stream.Results() {
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
				data := make([]byte, 640)
				binary.LittleEndian.PutUint16(data[0:2], uint16(i))
				err := stream.Feed(context.Background(), domain.AudioChunk{Data: data})
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, ErrStreamClosed):
					// Not accepted; must not reach the provider.
				default:
					t.Errorf("unexpected Feed error: %v", err)
				}
			}(i)
		}
		feedWG.Wait()
		closeWG.Wait()

		_, _, _, _, audio, _ := c.get()
		if int64(len(audio)) != accepted.Load() {
			t.Fatalf("iteration %d: delivered %d chunks, accepted %d (audio lost or duplicated)", iter, len(audio), accepted.Load())
		}
	}
}

// TestRealtimeAbnormalCloseAfterDone verifies that a provider closing the socket without a
// clean 1000 close after transcription.done is still a successful end.
func TestRealtimeAbnormalCloseAfterDone(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // append
		_ = wsReadMap(t, conn) // end
		wsWrite(t, conn, map[string]any{"type": "transcription.done", "text": "ok"})
		_ = conn.CloseNow() // abnormal, no close frame
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
		t.Fatalf("Close = %v, want nil after done", err)
	}
	evs := <-done
	if len(evs) == 0 || !evs[len(evs)-1].Final || evs[len(evs)-1].Text != "ok" {
		t.Fatalf("events = %+v", evs)
	}
}

// TestRealtimeCloseContextDeadline verifies Close honors a canceled/deadline context rather
// than waiting the full final drain.
func TestRealtimeCloseContextDeadline(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *realtimeCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // append
		_ = wsReadMap(t, conn) // end
		// Never send done, never close; Close must honor its context.
		<-conn.CloseRead(context.Background()).Done()
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

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = stream.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context deadline/canceled", err)
	}
}

// TestConcurrentClose ensures repeated concurrent Close calls share one result without a
// race or panic.
func TestConcurrentClose(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
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
