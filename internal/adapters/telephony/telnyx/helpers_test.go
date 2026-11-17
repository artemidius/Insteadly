package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const (
	testAPIKey       = "KEY_test_telnyx_api_key"
	testConnectionID = "conn_1234567890"
	testFromNumber   = "+15551234567"
	testCallControl  = "v3:test-call-control-id"
	testPublicHost   = "public.example.test"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testOrder() domain.Order {
	return domain.Order{
		ID:     domain.NewOrderID(),
		Clinic: domain.Clinic{Name: "Praxis Test", Phone: "+436641234567"},
	}
}

// fakeAPI is an in-repo stand-in for api.telnyx.com/v2. It records every request and
// never touches the network.
type fakeAPI struct {
	mu        sync.Mutex
	dial      dialRequest
	streaming streamingStartRequest
	dtmf      sendDTMFRequest
	hangups   int
	streams   int
	authOK    bool
	failWith  *APIError
	ccid      string
}

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		f.mu.Lock()
		f.authOK = false
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusUnauthorized, map[string]any{"errors": []any{map[string]any{"code": "401", "title": "unauthorized"}}})
		return
	}
	f.mu.Lock()
	f.authOK = true
	failWith := f.failWith
	f.mu.Unlock()
	if failWith != nil {
		writeJSONResponse(w, failWith.StatusCode, map[string]any{"errors": []any{map[string]any{
			"code": failWith.Code, "title": failWith.Title, "detail": failWith.Detail,
		}}})
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	path := r.URL.Path

	switch {
	case path == "/v2/calls" && r.Method == http.MethodPost:
		var req dialRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.dial = req
		ccid := f.ccid
		if ccid == "" {
			ccid = testCallControl
		}
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, map[string]any{"data": map[string]any{
			"call_control_id": ccid,
			"call_leg_id":     "leg-1",
			"call_session_id": "session-1",
			"is_alive":        false,
		}})
	case strings.HasSuffix(path, "/actions/streaming_start"):
		var req streamingStartRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.streaming = req
		f.streams++
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	case strings.HasSuffix(path, "/actions/send_dtmf"):
		var req sendDTMFRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.dtmf = req
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	case strings.HasSuffix(path, "/actions/hangup"):
		f.mu.Lock()
		f.hangups++
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	default:
		http.NotFound(w, r)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{}
	ts := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(ts.Close)
	return f, ts
}

// newTestAdapter points both the REST base and the public origin at test servers.
func newTestAdapter(t *testing.T, apiBase, publicBase string) *Adapter {
	t.Helper()
	if apiBase == "" {
		apiBase = "https://api.example.test"
	}
	if publicBase == "" {
		publicBase = "https://" + testPublicHost
	}
	a, err := New(Config{
		APIKey:        testAPIKey,
		ConnectionID:  testConnectionID,
		FromNumber:    testFromNumber,
		PublicBaseURL: publicBase,
		APIBaseURL:    apiBase,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func nextEvent(t *testing.T, ch <-chan domain.CallEvent) domain.CallEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for call event")
	}
	return domain.CallEvent{}
}

func wantEvent(t *testing.T, ev domain.CallEvent, kind domain.CallEventKind, reason domain.EndReason) {
	t.Helper()
	if ev.Kind != kind {
		t.Fatalf("event kind = %q, want %q (event %+v)", ev.Kind, kind, ev)
	}
	if reason != "" && ev.EndReason != reason {
		t.Fatalf("end reason = %q, want %q", ev.EndReason, reason)
	}
}

func drainEvents(s interface {
	Events() <-chan domain.CallEvent
}) {
	for range s.Events() {
	}
}

// waitFor polls until pred is true or the deadline passes.
func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// webhookBody builds a verified Telnyx call-control envelope.
func webhookBody(eventType, ccid, connectionID string, extra map[string]any) []byte {
	payload := map[string]any{
		"call_control_id": ccid,
		"connection_id":   connectionID,
	}
	for k, v := range extra {
		payload[k] = v
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{
		"event_type":  eventType,
		"id":          "evt-1",
		"occurred_at": "2026-11-17T22:00:00Z",
		"record_type": "event",
		"payload":     payload,
	}})
	return raw
}

// base64Sign reproduces the documented Telnyx Ed25519 signature:
// `{timestamp}|{payload}`, base64-encoded over the account key pair.
func base64Sign(t *testing.T, priv ed25519.PrivateKey, timestamp string, body []byte) string {
	t.Helper()
	msg := append([]byte(timestamp+"|"), body...)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

func signBody(t *testing.T, priv ed25519.PrivateKey, timestamp string, body []byte) (string, string) {
	t.Helper()
	return base64Sign(t, priv, timestamp, body), timestamp
}

func newSigningKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, *Adapter) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	publicB64 := base64.StdEncoding.EncodeToString(pub)
	a, err := New(Config{
		APIKey:           testAPIKey,
		ConnectionID:     testConnectionID,
		FromNumber:       testFromNumber,
		PublicBaseURL:    "https://" + testPublicHost,
		WebhookPublicKey: publicB64,
		APIBaseURL:       "https://api.example.test",
		Logger:           testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return pub, priv, a
}

func nowTimestamp() string { return strconv.FormatInt(time.Now().Unix(), 10) }
