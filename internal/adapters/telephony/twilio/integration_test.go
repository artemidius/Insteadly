package twilio

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// TestFakeCallEndToEnd exercises the whole adapter against a fake Twilio REST API and a
// fake Media Streams client: create -> status callbacks -> TwiML -> audio in/out -> DTMF
// redirect -> completion. It needs no credentials or external network.
func TestFakeCallEndToEnd(t *testing.T) {
	f, api := newFakeAPI(t)
	a := newTestAdapter(t, api.URL)
	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)

	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	status := func(status string) *httptest.ResponseRecorder {
		return formRequest(a, http.MethodPost, statusPath+string(s.id), url.Values{
			"CallSid": {testCallSID}, "CallStatus": {status},
		}, true)
	}
	if rec := status("ringing"); rec.Code != http.StatusOK {
		t.Fatalf("ringing callback = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")

	voice := formRequest(a, http.MethodPost, voicePath+string(s.id), url.Values{
		"CallSid": {testCallSID}, "AccountSid": {testAccountSID}, "CallStatus": {"in-progress"},
	}, true)
	if voice.Code != http.StatusOK || !strings.Contains(voice.Body.String(), "wss://") {
		t.Fatalf("voice webhook = %d %s", voice.Code, voice.Body.String())
	}

	c := dialStream(t, ts, s)
	writeJSON(t, c, streamStart(s))
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	writeJSON(t, c, map[string]any{
		"event": "media",
		"media": mediaPayload{
			Track: "inbound", Chunk: "1", Timestamp: "20",
			Payload: base64.StdEncoding.EncodeToString(make([]byte, twilioFrameBytes)),
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

	audio := make([]byte, domain.FrameBytes)
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: audio}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if msg := readJSON(t, c); msg.Event != "media" || len(msg.Media.Payload) == 0 {
		t.Fatalf("outbound media = %+v", msg)
	}

	if err := s.SendDTMF(context.Background(), "1"); err != nil {
		t.Fatalf("SendDTMF: %v", err)
	}
	f.mu.Lock()
	twiml := f.update.Get("Twiml")
	f.mu.Unlock()
	if !strings.Contains(twiml, `<Play digits="1">`) {
		t.Fatalf("DTMF update did not contain Play digits: %s", twiml)
	}

	// The redirect stops the old stream and reconnects a new one; the call stays alive.
	writeJSON(t, c, map[string]any{"event": "stop"})
	time.Sleep(50 * time.Millisecond)
	c2 := dialStream(t, ts, s)
	writeJSON(t, c2, streamStart(s))

	if rec := status("completed"); rec.Code != http.StatusOK {
		t.Fatalf("completed callback = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndCompleted)
	if _, ok := <-s.Events(); ok {
		t.Fatal("events channel still open after ended")
	}
}
