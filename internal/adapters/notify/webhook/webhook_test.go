package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func testNotification() domain.Notification {
	return domain.Notification{
		OrderID: "0123456789abcdef0123456789abcdef",
		Channel: "sms",
		To:      "+436641234567",
		Subject: "Termin bestätigt",
		Body:    "Ihr Termin am 20.11.2026",
	}
}

func TestNewValidates(t *testing.T) {
	cases := map[string]struct{ url, channel string }{
		"empty url":     {"", "email"},
		"relative url":  {"/hooks", "email"},
		"ftp scheme":    {"ftp://example.test/hook", "email"},
		"bad channel":   {"https://example.test/hook", "push"},
		"empty channel": {"https://example.test/hook", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(tc.url, tc.channel, nil); err == nil {
				t.Fatalf("New(%q,%q) accepted", tc.url, tc.channel)
			}
		})
	}
	if _, err := New("https://example.test/hook", "email", nil); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
}

func TestSendPostsJSON(t *testing.T) {
	var got payload
	var contentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(ts.Close)

	n, err := New(ts.URL, "email", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}
	if got.OrderID != "0123456789abcdef0123456789abcdef" || got.To != "+436641234567" ||
		got.Subject != "Termin bestätigt" || got.Body != "Ihr Termin am 20.11.2026" {
		t.Errorf("payload = %+v", got)
	}
	// A per-notification channel wins over the configured default.
	if got.Channel != "sms" {
		t.Errorf("channel = %q, want sms (notification passthrough)", got.Channel)
	}
}

func TestSendUsesConfiguredChannelWhenAbsent(t *testing.T) {
	var got payload
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	t.Cleanup(ts.Close)

	n, err := New(ts.URL, "sms", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	notification := testNotification()
	notification.Channel = ""
	if err := n.Send(context.Background(), notification); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Channel != "sms" {
		t.Fatalf("channel = %q, want the configured sms", got.Channel)
	}
}

func TestSendNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "sensitive upstream detail")
	}))
	t.Cleanup(ts.Close)

	n, _ := New(ts.URL, "email", nil)
	err := n.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if strings.Contains(err.Error(), "sensitive upstream detail") {
		t.Fatal("error leaked the response body")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want the status code", err)
	}
}

func TestSendTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	t.Cleanup(ts.Close)

	n, err := NewWithClient(ts.URL, "email", &http.Client{Timeout: 30 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if err := n.Send(context.Background(), testNotification()); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestSendHonorsCanceledContext(t *testing.T) {
	n, _ := New("https://example.test/hook", "email", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Send(ctx, testNotification()); err == nil {
		t.Fatal("expected context error")
	}
}

func TestSendDoesNotLeakURLInErrors(t *testing.T) {
	// A connection failure error must not include the secret URL.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL + "/super-secret-path"
	ts.Close()
	n, _ := New(url, "email", nil)
	err := n.Send(context.Background(), testNotification())
	if err != nil && strings.Contains(err.Error(), "super-secret-path") {
		t.Fatalf("error leaked the webhook URL: %v", err)
	}
}
