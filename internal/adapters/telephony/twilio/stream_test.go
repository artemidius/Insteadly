package twilio

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func newStreamServer(t *testing.T) (*Adapter, *httptest.Server) {
	t.Helper()
	_, api := newFakeAPI(t)
	a := newTestAdapter(t, api.URL)
	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)
	return a, ts
}

func dialStream(t *testing.T, ts *httptest.Server, s *session) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + streamPath + string(s.id)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func streamStart(s *session) map[string]any {
	return map[string]any{
		"event": "start",
		"start": startPayload{
			StreamSID:  "MZteststream",
			AccountSID: testAccountSID,
			CallSID:    testCallSID,
			MediaFormat: mediaFormat{
				Encoding:   "audio/x-mulaw",
				SampleRate: 8000,
				Channels:   1,
			},
			CustomParameters: map[string]string{callIDCustomParam: string(s.id)},
		},
	}
}

func writeJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, v); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func readJSON(t *testing.T, c *websocket.Conn) outboundMedia {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var msg outboundMedia
	if err := wsjson.Read(ctx, c, &msg); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return msg
}

func TestStreamInboundAudioAndDTMF(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	c := dialStream(t, ts, s)
	writeJSON(t, c, map[string]any{"event": "connected"})
	writeJSON(t, c, streamStart(s))

	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	// 160 µ-law bytes (20 ms) of the -32124 code -> 640 bytes of canonical PCM16.
	payload := base64.StdEncoding.EncodeToString(make([]byte, 160))
	writeJSON(t, c, map[string]any{
		"event": "media",
		"media": mediaPayload{Track: "inbound", Chunk: "1", Timestamp: "20", Payload: payload},
	})
	select {
	case chunk := <-s.Inbound():
		if len(chunk.Data) != domain.FrameBytes {
			t.Fatalf("inbound chunk length = %d, want %d", len(chunk.Data), domain.FrameBytes)
		}
		if got := int16(binary.LittleEndian.Uint16(chunk.Data[:2])); got != decodeMuLaw(0x00) {
			t.Fatalf("first sample = %d, want %d", got, decodeMuLaw(0x00))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound audio")
	}

	writeJSON(t, c, map[string]any{"event": "dtmf", "dtmf": dtmfPayload{Track: "inbound", Digit: "5"}})
	ev := nextEvent(t, s.Events())
	wantEvent(t, ev, domain.EventDTMF, "")
	if ev.DTMF != "5" {
		t.Fatalf("dtmf = %q, want 5", ev.DTMF)
	}
}

func TestStreamSendAudio(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	c := dialStream(t, ts, s)
	writeJSON(t, c, streamStart(s))
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	const sample = int16(1000)
	chunk := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(chunk[2*i:], uint16(sample))
	}
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: chunk}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}

	msg := readJSON(t, c)
	if msg.Event != "media" || msg.StreamSID != "MZteststream" {
		t.Fatalf("unexpected media message: %+v", msg)
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Media.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(raw) != twilioFrameBytes {
		t.Fatalf("payload bytes = %d, want %d", len(raw), twilioFrameBytes)
	}
	got := int(decodeMuLaw(raw[0]))
	if got < 800 || got > 1200 {
		t.Fatalf("decoded sample = %d, want about %d", got, sample)
	}
}

func TestStreamReconnectAndStop(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	c1 := dialStream(t, ts, s)
	writeJSON(t, c1, streamStart(s))
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	// A stop is not a call end: the stream may reconnect (e.g. DTMF redirect).
	writeJSON(t, c1, map[string]any{"event": "stop"})
	time.Sleep(50 * time.Millisecond)
	if s.isEnded() {
		t.Fatal("stream stop ended the call")
	}

	c2 := dialStream(t, ts, s)
	writeJSON(t, c2, streamStart(s))
	payload := base64.StdEncoding.EncodeToString(make([]byte, 160))
	writeJSON(t, c2, map[string]any{
		"event": "media",
		"media": mediaPayload{Track: "inbound", Chunk: "1", Timestamp: "20", Payload: payload},
	})
	select {
	case chunk := <-s.Inbound():
		if len(chunk.Data) != domain.FrameBytes {
			t.Fatalf("reconnected inbound length = %d", len(chunk.Data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for audio after reconnect")
	}
}

func TestStreamRejectsImpersonation(t *testing.T) {
	a, ts := newStreamServer(t)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	c := dialStream(t, ts, s)
	bad := streamStart(s)
	start := bad["start"].(startPayload)
	start.AccountSID = "AC" + strings.Repeat("f", 32)
	bad["start"] = start
	writeJSON(t, c, bad)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ignored any
	if err := wsjson.Read(ctx, c, &ignored); err == nil {
		t.Fatal("server kept an impersonated stream open")
	}
	if s.isAnswered() {
		t.Fatal("impersonated stream must not mark the call answered")
	}
}
