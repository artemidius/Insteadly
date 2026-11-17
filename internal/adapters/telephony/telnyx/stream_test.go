package telnyx

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/artemidius/insteadly/internal/core/domain"
)

type outboundMessage struct {
	Event string `json:"event"`
	Media struct {
		Payload string `json:"payload"`
	} `json:"media"`
	Mark struct {
		Name string `json:"name"`
	} `json:"mark"`
}

// newStreamServer wires an adapter whose public origin is the same httptest server that
// serves its handler, so the generated stream_url is exactly what the test connects to.
func newStreamServer(t *testing.T) (*Adapter, *fakeAPI, *httptest.Server) {
	t.Helper()
	f, api := newFakeAPI(t)
	var a *Adapter
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	a = newTestAdapter(t, api.URL, ts.URL)
	return a, f, ts
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

func streamStartEnvelope() map[string]any {
	return map[string]any{
		"event":     "start",
		"stream_id": "stream-1",
		"start": startPayload{
			CallControlID: testCallControl,
			MediaFormat: mediaFormat{
				Encoding:   "PCMU",
				SampleRate: 8000,
				Channels:   1,
			},
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

func readOutbound(t *testing.T, c *websocket.Conn) outboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var msg outboundMessage
	if err := wsjson.Read(ctx, c, &msg); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return msg
}

// dialAnswered starts a call, delivers call.answered (which triggers streaming_start),
// and attaches a media stream.
func dialAnswered(t *testing.T, a *Adapter, ts *httptest.Server) *session {
	t.Helper()
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	rec := postWebhook(t, a, s.id, webhookBody("call.answered", testCallControl, testConnectionID, nil), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call.answered = %d", rec.Code)
	}
	return s
}

func startStream(t *testing.T, ts *httptest.Server, s *session) *websocket.Conn {
	t.Helper()
	c := dialStream(t, ts, s)
	writeJSON(t, c, map[string]any{"event": "connected", "version": "1.0.0"})
	writeJSON(t, c, streamStartEnvelope())
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")
	return c
}

func TestStreamInboundAudio(t *testing.T) {
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)
	c := startStream(t, ts, s)

	// 160 µ-law bytes (20 ms) of code 0x00 -> 640 bytes of canonical PCM16.
	payload := base64.StdEncoding.EncodeToString(make([]byte, telnyxFrameBytes))
	writeJSON(t, c, map[string]any{
		"event":     "media",
		"stream_id": "stream-1",
		"media":     mediaPayload{Track: "inbound", Chunk: "1", Timestamp: "20", Payload: payload},
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

	// The outbound leg of a both_tracks stream is not far-end audio.
	writeJSON(t, c, map[string]any{
		"event": "media",
		"media": mediaPayload{Track: "outbound", Payload: payload},
	})
	select {
	case chunk := <-s.Inbound():
		t.Fatalf("outbound track was captured as inbound audio: %d bytes", len(chunk.Data))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStreamDTMF(t *testing.T) {
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)
	c := startStream(t, ts, s)

	writeJSON(t, c, map[string]any{"event": "dtmf", "stream_id": "stream-1", "dtmf": dtmfPayload{Digit: "5"}})
	ev := nextEvent(t, s.Events())
	wantEvent(t, ev, domain.EventDTMF, "")
	if ev.DTMF != "5" {
		t.Fatalf("dtmf = %q, want 5", ev.DTMF)
	}
	// A malformed digit is ignored, never forwarded.
	writeJSON(t, c, map[string]any{"event": "dtmf", "dtmf": dtmfPayload{Digit: "✱"}})
	writeJSON(t, c, map[string]any{"event": "dtmf", "dtmf": dtmfPayload{Digit: "1"}})
	if ev := nextEvent(t, s.Events()); ev.Kind != domain.EventDTMF || ev.DTMF != "1" {
		t.Fatalf("after malformed digit got %+v, want dtmf 1", ev)
	}
}

func TestStreamSendAudioClearAndMarks(t *testing.T) {
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)
	c := startStream(t, ts, s)

	const sample = int16(1000)
	chunk := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		binary.LittleEndian.PutUint16(chunk[2*i:], uint16(sample))
	}
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: chunk}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	msg := readOutbound(t, c)
	if msg.Event != "media" {
		t.Fatalf("outbound event = %q, want media", msg.Event)
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Media.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(raw) != telnyxFrameBytes {
		t.Fatalf("payload bytes = %d, want %d", len(raw), telnyxFrameBytes)
	}
	if got := int(decodeMuLaw(raw[0])); got < 800 || got > 1200 {
		t.Fatalf("decoded sample = %d, want about %d", got, sample)
	}

	// A mark is written to the wire and acknowledged by the provider echo.
	if err := s.MarkPlayback(context.Background(), "mark-1"); err != nil {
		t.Fatalf("MarkPlayback: %v", err)
	}
	msg = readOutbound(t, c)
	if msg.Event != "mark" || msg.Mark.Name != "mark-1" {
		t.Fatalf("outbound mark = %+v", msg)
	}
	writeJSON(t, c, map[string]any{"event": "mark", "stream_id": "stream-1", "mark": markPayload{Name: "mark-1"}})
	ev := nextEvent(t, s.Events())
	wantEvent(t, ev, domain.EventPlaybackMark, "")
	if ev.PlaybackID != "mark-1" {
		t.Fatalf("playback id = %q", ev.PlaybackID)
	}

	// Clear is a wire message, ordered after the queued media.
	if err := s.ClearAudio(context.Background()); err != nil {
		t.Fatalf("ClearAudio: %v", err)
	}
	msg = readOutbound(t, c)
	if msg.Event != "clear" {
		t.Fatalf("outbound event = %q, want clear", msg.Event)
	}
}

func TestStreamStopDoesNotEndCall(t *testing.T) {
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)
	c := startStream(t, ts, s)

	writeJSON(t, c, map[string]any{"event": "stop", "stream_id": "stream-1"})
	time.Sleep(50 * time.Millisecond)
	if s.isEnded() {
		t.Fatal("stream stop ended the call")
	}

	// A reconnect attaches a new stream; the call is already answered, so no events.
	c2 := dialStream(t, ts, s)
	writeJSON(t, c2, streamStartEnvelope())
	payload := base64.StdEncoding.EncodeToString(make([]byte, telnyxFrameBytes))
	writeJSON(t, c2, map[string]any{"event": "media", "media": mediaPayload{Track: "inbound", Payload: payload}})
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
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)

	c := dialStream(t, ts, s)
	bad := streamStartEnvelope()
	start := bad["start"].(startPayload)
	start.CallControlID = "v3:not-ours"
	bad["start"] = start
	writeJSON(t, c, bad)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ignored any
	if err := wsjson.Read(ctx, c, &ignored); err == nil {
		t.Fatal("server kept an impersonated stream open")
	}
	select {
	case <-s.StreamReady():
		t.Fatal("impersonated stream must not be accepted")
	default:
	}
}

func TestStreamRejectsUnexpectedMediaFormat(t *testing.T) {
	a, _, ts := newStreamServer(t)
	s := dialAnswered(t, a, ts)
	c := dialStream(t, ts, s)

	bad := streamStartEnvelope()
	start := bad["start"].(startPayload)
	start.MediaFormat.Encoding = "PCMA"
	bad["start"] = start
	writeJSON(t, c, bad)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ignored any
	if err := wsjson.Read(ctx, c, &ignored); err == nil {
		t.Fatal("server accepted a non-PCMU stream")
	}
	select {
	case <-s.StreamReady():
		t.Fatal("rejected stream must not be accepted")
	default:
	}
}

func TestStreamHandshakeSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f, api := newFakeAPI(t)
	var a *Adapter
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	a, err = New(Config{
		APIKey:           testAPIKey,
		ConnectionID:     testConnectionID,
		FromNumber:       testFromNumber,
		PublicBaseURL:    ts.URL,
		WebhookPublicKey: base64.StdEncoding.EncodeToString(pub),
		APIBaseURL:       api.URL,
		Logger:           testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = f
	s := dialAnswered(t, a, ts)

	// A valid handshake over `{timestamp}|{stream_url}` is accepted.
	streamURL := a.streamURL(s.id)
	tsHdr := nowTimestamp()
	sig := base64Sign(t, priv, tsHdr, []byte(streamURL))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + streamPath + string(s.id)
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: map[string][]string{
		signatureHeader: {sig},
		timestampHeader: {tsHdr},
	}})
	if err != nil {
		t.Fatalf("signed handshake rejected: %v", err)
	}
	_ = c.CloseNow()

	// A bad signature is refused before the upgrade.
	bad, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: map[string][]string{
		signatureHeader: {"AAAA"},
		timestampHeader: {tsHdr},
	}})
	if err == nil {
		_ = bad.CloseNow()
		t.Fatal("bad handshake signature accepted")
	}
}
