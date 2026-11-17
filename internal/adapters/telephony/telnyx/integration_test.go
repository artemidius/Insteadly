package telnyx

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// TestFakeCallEndToEnd exercises the whole adapter against a fake Telnyx REST API and a
// loopback media-stream client: dial -> call.initiated -> call.answered -> streaming_start
// -> WS start -> audio in/out -> DTMF -> mark echo -> call.hangup. It needs no account,
// key, or external network.
func TestFakeCallEndToEnd(t *testing.T) {
	a, f, ts := newStreamServer(t)

	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	if rec := postWebhook(t, a, s.id, webhookBody("call.initiated", testCallControl, testConnectionID, map[string]any{
		"direction": "outgoing",
	}), nil); rec.Code != http.StatusOK {
		t.Fatalf("call.initiated = %d", rec.Code)
	}

	if rec := postWebhook(t, a, s.id, webhookBody("call.answered", testCallControl, testConnectionID, nil), nil); rec.Code != http.StatusOK {
		t.Fatalf("call.answered = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")
	waitFor(t, "streaming_start", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.streams == 1
	})

	c := dialStream(t, ts, s)
	writeJSON(t, c, streamStartEnvelope())

	// Inbound canonical audio from a 160-byte µ-law frame.
	writeJSON(t, c, map[string]any{
		"event": "media",
		"media": mediaPayload{
			Track: "inbound", Chunk: "1", Timestamp: "20",
			Payload: base64.StdEncoding.EncodeToString(make([]byte, telnyxFrameBytes)),
		},
	})
	select {
	case chunk := <-s.Inbound():
		if len(chunk.Data) != domain.FrameBytes {
			t.Fatalf("inbound chunk = %d bytes", len(chunk.Data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound audio")
	}

	// Outbound canonical audio becomes a µ-law media frame.
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if msg := readOutbound(t, c); msg.Event != "media" || msg.Media.Payload == "" {
		t.Fatalf("outbound media = %+v", msg)
	}

	// Outbound DTMF uses the Call Control action.
	if err := s.SendDTMF(context.Background(), "1w2#"); err != nil {
		t.Fatalf("SendDTMF: %v", err)
	}
	waitFor(t, "send_dtmf", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.dtmf.Digits == "1w2#"
	})

	// Playback mark round trip.
	if err := s.MarkPlayback(context.Background(), "mark-e2e"); err != nil {
		t.Fatalf("MarkPlayback: %v", err)
	}
	if msg := readOutbound(t, c); msg.Event != "mark" || msg.Mark.Name != "mark-e2e" {
		t.Fatalf("outbound mark = %+v", msg)
	}
	writeJSON(t, c, map[string]any{"event": "mark", "mark": markPayload{Name: "mark-e2e"}})
	if ev := nextEvent(t, s.Events()); ev.Kind != domain.EventPlaybackMark || ev.PlaybackID != "mark-e2e" {
		t.Fatalf("playback mark event = %+v", ev)
	}

	// Natural completion via the terminal webhook.
	if rec := postWebhook(t, a, s.id, webhookBody("call.hangup", testCallControl, testConnectionID, map[string]any{
		"hangup_cause": "normal_clearing",
	}), nil); rec.Code != http.StatusOK {
		t.Fatalf("call.hangup = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndCompleted)
	if _, ok := <-s.Events(); ok {
		t.Fatal("events channel still open after ended")
	}
	if _, ok := <-s.Inbound(); ok {
		t.Fatal("inbound channel still open after ended")
	}
}
