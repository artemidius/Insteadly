package telnyx

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestDialCreateCall(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL, "")
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Hangup(context.Background()); drainEvents(sess) })

	f.mu.Lock()
	req := f.dial
	authOK := f.authOK
	f.mu.Unlock()
	if !authOK {
		t.Fatal("Telnyx API was not called with bearer auth")
	}
	if req.To != "+436641234567" {
		t.Errorf("to = %q", req.To)
	}
	if req.From != testFromNumber {
		t.Errorf("from = %q", req.From)
	}
	if req.ConnectionID != testConnectionID {
		t.Errorf("connection_id = %q", req.ConnectionID)
	}
	if req.WebhookURL != a.webhookURL(sess.ID()) {
		t.Errorf("webhook_url = %q, want %q", req.WebhookURL, a.webhookURL(sess.ID()))
	}
	if req.TimeoutSecs != 0 {
		t.Errorf("timeout_secs = %d, want omitted by default", req.TimeoutSecs)
	}

	wantEvent(t, nextEvent(t, sess.Events()), domain.EventDialing, "")
}

func TestDialRingTimeout(t *testing.T) {
	f, ts := newFakeAPI(t)
	a, err := New(Config{
		APIKey:        testAPIKey,
		ConnectionID:  testConnectionID,
		FromNumber:    testFromNumber,
		PublicBaseURL: "https://" + testPublicHost,
		APIBaseURL:    ts.URL,
		RingTimeout:   45 * time.Second,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Hangup(context.Background()); drainEvents(sess) })

	f.mu.Lock()
	got := f.dial.TimeoutSecs
	f.mu.Unlock()
	if got != 45 {
		t.Fatalf("timeout_secs = %d, want 45", got)
	}
}

func TestDialAPIError(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.failWith = &APIError{StatusCode: http.StatusUnprocessableEntity, Code: "10009", Title: "Invalid", Detail: "bad destination +431234"}
	a := newTestAdapter(t, ts.URL, "")

	_, err := a.Dial(context.Background(), testOrder())
	if err == nil {
		t.Fatal("expected Dial error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "10009" {
		t.Fatalf("err = %v, want APIError 10009", err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatal("error leaked the API key")
	}
	a.mu.Lock()
	left := len(a.sessions)
	a.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions left registered after failed dial", left)
	}
}

func TestDialRejectsInvalidPhone(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL, "")
	order := testOrder()
	order.Clinic.Phone = "0664 1234567"
	if _, err := a.Dial(context.Background(), order); err == nil {
		t.Fatal("expected invalid phone error")
	}
	f.mu.Lock()
	called := f.dial.To != ""
	f.mu.Unlock()
	if called {
		t.Fatal("Telnyx API must not be called for an invalid phone")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	base := Config{
		APIKey:        testAPIKey,
		ConnectionID:  testConnectionID,
		FromNumber:    testFromNumber,
		PublicBaseURL: "https://" + testPublicHost,
		APIBaseURL:    "https://api.example.test",
	}
	cases := map[string]func(*Config){
		"missing api key":    func(c *Config) { c.APIKey = "" },
		"missing connection": func(c *Config) { c.ConnectionID = "  " },
		"bad from":           func(c *Config) { c.FromNumber = "555" },
		"bad public url":     func(c *Config) { c.PublicBaseURL = "public.example.test" },
		"bad api url":        func(c *Config) { c.APIBaseURL = "::" },
		"ring too short":     func(c *Config) { c.RingTimeout = time.Second },
		"bad public key":     func(c *Config) { c.WebhookPublicKey = "not-base64!!" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted %s", name)
			}
		})
	}
}

func TestHangupTreatsAlreadyEndedAsSuccess(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL, "")
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)

	f.mu.Lock()
	f.failWith = &APIError{StatusCode: http.StatusUnprocessableEntity, Code: alreadyEndedCode}
	f.mu.Unlock()
	if err := s.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup with 90018 = %v, want nil", err)
	}
	if !s.isEnded() {
		t.Fatal("session must end locally")
	}
	drainEvents(s)
}

func TestHangupSendsEndCall(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL, "")
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	if err := s.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	f.mu.Lock()
	hangups := f.hangups
	f.mu.Unlock()
	if hangups != 1 {
		t.Fatalf("hangup API calls = %d, want 1", hangups)
	}
}
