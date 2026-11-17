package twilio

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const testCallSID = "CA0123456789abcdef0123456789abcdef"

func newWebhookSession(t *testing.T, a *Adapter) *session {
	t.Helper()
	s := newSession(a, domain.NewCallID())
	s.setProviderSID(testCallSID)
	a.add(s)
	t.Cleanup(func() {
		s.finish(domain.EndAborted)
		drainEvents(s)
	})
	return s
}

func formRequest(a *Adapter, method, path string, params url.Values, sign bool) *httptest.ResponseRecorder {
	body := ""
	contentType := ""
	if method == http.MethodPost {
		body = params.Encode()
		contentType = "application/x-www-form-urlencoded"
	}
	req := httptest.NewRequest(method, a.publicBaseURL+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if sign {
		req.Header.Set("X-Twilio-Signature", signTestForm(a.cfg.AuthToken, a.publicBaseURL+path, params))
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestVoiceWebhook(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)

	params := url.Values{
		"CallSid":    {testCallSID},
		"AccountSid": {testAccountSID},
		"CallStatus": {"in-progress"},
	}
	rec := formRequest(a, http.MethodPost, voicePath+string(s.id), params, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<Connect>") || !strings.Contains(rec.Body.String(), "<Stream") {
		t.Errorf("unexpected TwiML: %s", rec.Body.String())
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)
	params := url.Values{"CallSid": {testCallSID}, "CallStatus": {"ringing"}}

	if rec := formRequest(a, http.MethodPost, voicePath+string(s.id), params, false); rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned status = %d, want 403", rec.Code)
	}

	// Sign one body, send another.
	req := httptest.NewRequest(http.MethodPost, a.publicBaseURL+voicePath+string(s.id), strings.NewReader("CallSid=evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", signTestForm(a.cfg.AuthToken, a.publicBaseURL+voicePath+string(s.id), params))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tampered status = %d, want 403", rec.Code)
	}
}

func TestWebhookSharedKeySettingsRejected(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)
	params := url.Values{"CallSid": {testCallSID}, "CallStatus": {"ringing"}}
	req := httptest.NewRequest(http.MethodPost, a.publicBaseURL+statusPath+string(s.id), strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", signTestForm(a.cfg.AuthToken, a.publicBaseURL+statusPath+string(s.id), params))
	req.Header.Set("X-Twilio-Webhooks-Signature-Settings", "key=abc,algorithm=HMAC_SHA256")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestVoiceWebhookUnknownAndEnded(t *testing.T) {
	a := newTestAdapter(t, "")
	params := url.Values{
		"CallSid": {testCallSID}, "AccountSid": {testAccountSID}, "CallStatus": {"in-progress"},
	}
	unknown := domain.NewCallID()
	if rec := formRequest(a, http.MethodPost, voicePath+string(unknown), params, true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown call status = %d, want 404", rec.Code)
	}

	s := newWebhookSession(t, a)
	s.finish(domain.EndAborted)
	rec := formRequest(a, http.MethodPost, voicePath+string(s.id), params, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<Hangup>") {
		t.Fatalf("ended call response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusWebhook(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)

	params := url.Values{"CallSid": {testCallSID}, "CallStatus": {"ringing"}}
	if rec := formRequest(a, http.MethodPost, statusPath+string(s.id), params, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")

	params = url.Values{"CallSid": {testCallSID}, "CallStatus": {"completed"}}
	if rec := formRequest(a, http.MethodPost, statusPath+string(s.id), params, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndCompleted)

	// Unknown calls (including already-removed ones) are acknowledged, not retried.
	unknown := domain.NewCallID()
	if rec := formRequest(a, http.MethodPost, statusPath+string(unknown), params, true); rec.Code != http.StatusOK {
		t.Fatalf("unknown status = %d, want 200", rec.Code)
	}
}

func TestStreamStatusWebhook(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)
	params := url.Values{
		"CallSid":     {testCallSID},
		"StreamSid":   {"MZabcdef"},
		"StreamEvent": {"stream-started"},
	}
	if rec := formRequest(a, http.MethodPost, streamStatusPath+string(s.id), params, true); rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d", rec.Code)
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)
	params := url.Values{"CallSid": {testCallSID}, "CallStatus": {"ringing"}, "padding": {strings.Repeat("x", 70<<10)}}
	rec := formRequest(a, http.MethodPost, statusPath+string(s.id), params, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestVoiceUnsignedRequestRequiresMatchingCall(t *testing.T) {
	a := newTestAdapter(t, "")
	s := newWebhookSession(t, a)
	params := url.Values{
		"CallSid": {testCallSID}, "AccountSid": {testAccountSID}, "CallStatus": {"in-progress"},
	}

	// Twilio's TwiML fetch is unsigned; the matching call ID + AccountSid/CallSid guard it.
	rec := formRequest(a, http.MethodPost, voicePath+string(s.id), params, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("unsigned voice request = %d, want 200", rec.Code)
	}

	bad := url.Values{
		"CallSid": {testCallSID}, "AccountSid": {"AC" + strings.Repeat("f", 32)},
	}
	rec = formRequest(a, http.MethodPost, voicePath+string(s.id), bad, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong AccountSid = %d, want 403", rec.Code)
	}

	bad = url.Values{
		"CallSid": {"CAffffffffffffffffffffffffffffffff"}, "AccountSid": {testAccountSID},
	}
	rec = formRequest(a, http.MethodPost, voicePath+string(s.id), bad, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong CallSid = %d, want 403", rec.Code)
	}
}
