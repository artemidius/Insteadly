package twilio

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func readRaw(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var v map[string]any
	if err := wsjson.Read(ctx, c, &v); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return v
}

func attachStream(t *testing.T, a *Adapter, ts *httptest.Server, s *session) *websocket.Conn {
	t.Helper()
	c := dialStream(t, ts, s)
	writeJSON(t, c, map[string]any{"event": "connected"})
	writeJSON(t, c, streamStart(s))
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")
	return c
}

func TestClearAudioAndMarkWireMessages(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	c := attachStream(t, a, ts, s)

	pcm := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(1000))
	}
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: pcm}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	media := readRaw(t, c)
	if media["event"] != "media" || media["streamSid"] != "MZteststream" {
		t.Fatalf("media message = %+v", media)
	}
	payload, _ := media["media"].(map[string]any)["payload"].(string)
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) != twilioFrameBytes {
		t.Fatalf("media payload = %d bytes err=%v", len(raw), err)
	}

	if err := s.MarkPlayback(context.Background(), "a1-2"); err != nil {
		t.Fatalf("MarkPlayback: %v", err)
	}
	mark := readRaw(t, c)
	if mark["event"] != "mark" || mark["streamSid"] != "MZteststream" {
		t.Fatalf("mark message = %+v", mark)
	}
	if got := mark["mark"].(map[string]any)["name"]; got != "a1-2" {
		t.Fatalf("mark name = %v", got)
	}

	if err := s.ClearAudio(context.Background()); err != nil {
		t.Fatalf("ClearAudio: %v", err)
	}
	clear := readRaw(t, c)
	if clear["event"] != "clear" || clear["streamSid"] != "MZteststream" {
		t.Fatalf("clear message = %+v", clear)
	}
	if _, ok := clear["mark"]; ok {
		t.Errorf("clear carries a mark: %+v", clear)
	}
}

func TestInboundMarkBecomesPlaybackEvent(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	c := attachStream(t, a, ts, s)

	writeJSON(t, c, map[string]any{"event": "mark", "mark": map[string]any{"name": "a1-1"}})
	ev := nextEvent(t, s.Events())
	if ev.Kind != domain.EventPlaybackMark || ev.PlaybackID != "a1-1" {
		t.Fatalf("mark event = %+v", ev)
	}

	// Malformed/empty marks are ignored, never emitted with an empty ID.
	writeJSON(t, c, map[string]any{"event": "mark", "mark": map[string]any{"name": ""}})
	select {
	case ev := <-s.Events():
		t.Fatalf("malformed mark produced event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMarkPlaybackWithoutStreamFailsHonestly(t *testing.T) {
	a, _ := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = s.MarkPlayback(ctx, "a1-1")
	if !errors.Is(err, ErrSessionNotReady) {
		t.Fatalf("MarkPlayback without stream = %v, want ErrSessionNotReady", err)
	}
}

func TestClearAudioWithoutStreamSucceedsAndPurges(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	// A partial chunk leaves encoder residual, then the writer blocks waiting for a stream.
	partial := make([]byte, 100)
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: partial}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.ClearAudio(ctx); err != nil {
		t.Fatalf("ClearAudio without stream = %v", err)
	}

	// Epoch purge: no old audio may surface when a stream attaches later.
	c := dialStream(t, ts, s)
	writeJSON(t, c, map[string]any{"event": "connected"})
	writeJSON(t, c, streamStart(s))
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	// Residual encoder bytes were dropped: the first media message after the clear must be
	// a full frame encoded from scratch, not a mixture with the pre-clear partial chunk.
	full := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(full[2*i:], uint16(1234))
	}
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: full}); err != nil {
		t.Fatalf("SendAudio after clear: %v", err)
	}
	msg := readRaw(t, c)
	got, _ := base64.StdEncoding.DecodeString(msg["media"].(map[string]any)["payload"].(string))
	var enc frameEncoder
	want := enc.encode(full)[0]
	if len(got) != len(want) {
		t.Fatalf("encoded length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("encoder residual leaked into post-clear audio at byte %d", i)
		}
	}
}

func TestClearAfterEndReturnsSessionEnded(t *testing.T) {
	a, _ := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	if err := s.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	drainEvents(s)
	if err := s.ClearAudio(context.Background()); !errors.Is(err, ErrSessionEnded) {
		t.Errorf("ClearAudio after end = %v, want ErrSessionEnded", err)
	}
	if err := s.MarkPlayback(context.Background(), "a1-1"); !errors.Is(err, ErrSessionEnded) {
		t.Errorf("MarkPlayback after end = %v, want ErrSessionEnded", err)
	}
}

func TestConcurrentPlaybackClearMarkHangup(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	c := attachStream(t, a, ts, s)
	// Drain the outbound socket so writer writes cannot block.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			var v map[string]any
			_ = wsjson.Read(ctx, c, &v)
			cancel()
		}
	}()
	t.Cleanup(func() { close(stop); _ = s.Hangup(context.Background()); drainEvents(s) })

	pcm := make([]byte, domain.FrameBytes)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.SendAudio(context.Background(), domain.AudioChunk{Data: pcm})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = s.ClearAudio(ctx)
				cancel()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = s.MarkPlayback(ctx, "stress-mark")
				cancel()
			}
		}()
	}
	wg.Wait()
	_ = s.Hangup(context.Background())
	drainEvents(s)
}
