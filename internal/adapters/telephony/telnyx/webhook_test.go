package telnyx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// newWebhookSession creates a live session registered with the adapter, without a real
// provider call.
func newWebhookSession(t *testing.T, a *Adapter) *session {
	t.Helper()
	s := newSession(a, domain.NewCallID())
	s.setProviderSID(testCallControl)
	a.add(s)
	t.Cleanup(func() {
		s.finish(domain.EndAborted)
		drainEvents(s)
	})
	return s
}

func postWebhook(t *testing.T, a *Adapter, id domain.CallID, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, a.publicBaseURL+webhookPath+string(id), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhookEventNormalization(t *testing.T) {
	a := newTestAdapter(t, "", "")
	s := newWebhookSession(t, a)

	rec := postWebhook(t, a, s.id, webhookBody("call.initiated", testCallControl, testConnectionID, map[string]any{
		"direction": "outgoing",
	}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call.initiated = %d %s", rec.Code, rec.Body.String())
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	rec = postWebhook(t, a, s.id, webhookBody("call.answered", testCallControl, testConnectionID, nil), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call.answered = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	rec = postWebhook(t, a, s.id, webhookBody("call.hangup", testCallControl, testConnectionID, map[string]any{
		"hangup_cause": "normal_clearing",
	}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call.hangup = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndCompleted)
	if _, ok := <-s.Events(); ok {
		t.Fatal("events channel still open after ended")
	}
}

func TestWebhookHangupCauses(t *testing.T) {
	cases := map[string]domain.EndReason{
		"normal_clearing":   domain.EndCompleted,
		"user_busy":         domain.EndBusy,
		"no_answer":         domain.EndNoAnswer,
		"timeout":           domain.EndNoAnswer,
		"call_rejected":     domain.EndRejected,
		"not_found":         domain.EndRejected,
		"originator_cancel": domain.EndAborted,
		"time_limit":        domain.EndError,
		"unspecified":       domain.EndError,
		"something_new":     domain.EndError,
	}
	for cause, want := range cases {
		t.Run(cause, func(t *testing.T) {
			a := newTestAdapter(t, "", "")
			s := newWebhookSession(t, a)
			rec := postWebhook(t, a, s.id, webhookBody("call.hangup", testCallControl, testConnectionID, map[string]any{
				"hangup_cause": cause,
			}), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			// progress fills dialing/ringing before the ended event when needed.
			for {
				ev := nextEvent(t, s.Events())
				if ev.Kind == domain.EventEnded {
					if ev.EndReason != want {
						t.Fatalf("end reason = %q, want %q", ev.EndReason, want)
					}
					return
				}
			}
		})
	}
}

func TestWebhookCapabilityChecks(t *testing.T) {
	a := newTestAdapter(t, "", "")

	t.Run("wrong connection id", func(t *testing.T) {
		s := newWebhookSession(t, a)
		rec := postWebhook(t, a, s.id, webhookBody("call.answered", testCallControl, "conn_other", nil), nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("wrong call control id", func(t *testing.T) {
		s := newWebhookSession(t, a)
		rec := postWebhook(t, a, s.id, webhookBody("call.answered", "v3:other", testConnectionID, nil), nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("missing call control id", func(t *testing.T) {
		s := newWebhookSession(t, a)
		rec := postWebhook(t, a, s.id, webhookBody("call.answered", "", testConnectionID, nil), nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("unknown call acknowledged", func(t *testing.T) {
		rec := postWebhook(t, a, domain.NewCallID(), webhookBody("call.hangup", testCallControl, testConnectionID, nil), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (Telnyx must stop retrying)", rec.Code)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		s := newWebhookSession(t, a)
		rec := postWebhook(t, a, s.id, []byte("{not json"), nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestWebhookSignature(t *testing.T) {
	_, priv, a := newSigningKey(t)
	s := newWebhookSession(t, a)
	body := webhookBody("call.initiated", testCallControl, testConnectionID, map[string]any{"direction": "outgoing"})
	ts := nowTimestamp()
	sig, ts := signBody(t, priv, ts, body)

	rec := postWebhook(t, a, s.id, body, map[string]string{
		signatureHeader: sig,
		timestampHeader: ts,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("signed webhook = %d %s", rec.Code, rec.Body.String())
	}

	t.Run("tampered body", func(t *testing.T) {
		tampered := webhookBody("call.hangup", testCallControl, testConnectionID, nil)
		rec := postWebhook(t, a, s.id, tampered, map[string]string{
			signatureHeader: sig,
			timestampHeader: ts,
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("stale timestamp", func(t *testing.T) {
		staleTS := "1000000000"
		staleSig := base64Sign(t, priv, staleTS, body)
		rec := postWebhook(t, a, s.id, body, map[string]string{
			signatureHeader: staleSig,
			timestampHeader: staleTS,
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("signature without timestamp", func(t *testing.T) {
		rec := postWebhook(t, a, s.id, body, map[string]string{signatureHeader: sig})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

func TestWebhookSignedButNoKeyFailsClosed(t *testing.T) {
	_, priv, _ := newSigningKey(t)
	a := newTestAdapter(t, "", "") // no WebhookPublicKey
	s := newWebhookSession(t, a)
	body := webhookBody("call.initiated", testCallControl, testConnectionID, map[string]any{"direction": "outgoing"})
	ts := nowTimestamp()
	sig := base64Sign(t, priv, ts, body)
	rec := postWebhook(t, a, s.id, body, map[string]string{
		signatureHeader: sig,
		timestampHeader: ts,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail closed without a configured key)", rec.Code)
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	a := newTestAdapter(t, "", "")
	s := newWebhookSession(t, a)
	rec := postWebhook(t, a, s.id, []byte(strings.Repeat("x", 70<<10)), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestWebhookTriggersStreamingStart(t *testing.T) {
	f, api := newFakeAPI(t)
	var a *Adapter
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	a = newTestAdapter(t, api.URL, ts.URL)

	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	<-s.Events() // dialing

	rec := postWebhook(t, a, s.id, webhookBody("call.answered", testCallControl, testConnectionID, nil), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call.answered = %d", rec.Code)
	}
	waitFor(t, "streaming_start", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.streams == 1
	})
	f.mu.Lock()
	req := f.streaming
	f.mu.Unlock()
	if req.StreamURL != a.streamURL(s.id) {
		t.Errorf("stream_url = %q, want %q", req.StreamURL, a.streamURL(s.id))
	}
	if req.StreamTrack != "both_tracks" {
		t.Errorf("stream_track = %q", req.StreamTrack)
	}
	if req.StreamBidirectionalMode != "rtp" || req.StreamBidirectionalCodec != "PCMU" {
		t.Errorf("bidirectional = %q/%q, want rtp/PCMU", req.StreamBidirectionalMode, req.StreamBidirectionalCodec)
	}
}

func TestInboundWebhookIsRejectedByHangup(t *testing.T) {
	f, api := newFakeAPI(t)
	a := newTestAdapter(t, api.URL, "")
	s := newWebhookSession(t, a)

	rec := postWebhook(t, a, s.id, webhookBody("call.initiated", testCallControl, testConnectionID, map[string]any{
		"direction": "incoming",
	}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	waitFor(t, "inbound hangup", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.hangups == 1
	})
	if !s.isEnded() {
		t.Fatal("unexpected inbound call must end the session")
	}
}
