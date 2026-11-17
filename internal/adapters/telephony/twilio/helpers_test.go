package twilio

import (
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const (
	testAccountSID = "AC" + "0123456789abcdef0123456789abcdef"
	testAuthToken  = "test-auth-token"
	testFromNumber = "+15551234567"
	testPublicBase = "https://public.example.test"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAdapter(t *testing.T, apiBase string) *Adapter {
	t.Helper()
	if apiBase == "" {
		apiBase = "https://api.example.test"
	}
	a, err := New(Config{
		AccountSID:    testAccountSID,
		AuthToken:     testAuthToken,
		FromNumber:    testFromNumber,
		PublicBaseURL: testPublicBase,
		APIBaseURL:    apiBase,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// signTestForm reproduces Twilio's signature so tests can authenticate requests. The
// independent published-vector test is what proves the validator itself.
func signTestForm(token, requestURL string, params url.Values) string {
	var b strings.Builder
	b.WriteString(requestURL)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ""
		if len(params[k]) > 0 {
			v = params[k][0]
		}
		b.WriteString(k)
		b.WriteString(v)
	}
	return signatureFor(token, b.String())
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
