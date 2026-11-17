package twilio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

type fakeAPI struct {
	mu        sync.Mutex
	create    url.Values
	update    url.Values
	end       url.Values
	authOK    bool
	failWith  *APIError
	createSID string
}

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	if user, pass, ok := r.BasicAuth(); !ok || user != testAccountSID || pass != testAuthToken {
		f.mu.Lock()
		f.authOK = false
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.authOK = true
	f.mu.Unlock()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/Calls.json") && r.Method == http.MethodPost {
		f.mu.Lock()
		f.create = r.PostForm
		failWith := f.failWith
		sid := f.createSID
		if sid == "" {
			sid = "CA0123456789abcdef0123456789abcdef"
		}
		f.mu.Unlock()
		if failWith != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failWith.StatusCode)
			_, _ = io.WriteString(w, `{"code":21215,"message":"blocked by geo permissions","more_info":"https://www.twilio.com/docs/errors/21215"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sid":"`+sid+`","status":"queued"}`)
		return
	}

	if strings.Contains(r.URL.Path, "/Calls/") && strings.HasSuffix(r.URL.Path, ".json") {
		f.mu.Lock()
		if r.PostForm.Get("Status") != "" {
			f.end = r.PostForm
		} else {
			f.update = r.PostForm
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sid":"CA0123456789abcdef0123456789abcdef","status":"in-progress"}`)
		return
	}

	http.NotFound(w, r)
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{}
	ts := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(ts.Close)
	return f, ts
}

func testOrder() domain.Order {
	return domain.Order{
		ID:     domain.NewOrderID(),
		Clinic: domain.Clinic{Name: "Praxis Test", Phone: "+436641234567"},
	}
}

func TestDialCreateCall(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL)

	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Hangup(context.Background()); drainEvents(sess) })

	f.mu.Lock()
	form := f.create
	authOK := f.authOK
	f.mu.Unlock()
	if !authOK {
		t.Error("Twilio API was not called with basic auth")
	}
	if got := form.Get("To"); got != "+436641234567" {
		t.Errorf("To = %q", got)
	}
	if got := form.Get("From"); got != testFromNumber {
		t.Errorf("From = %q", got)
	}
	if got := form.Get("Url"); got != a.voiceURL(sess.ID()) {
		t.Errorf("Url = %q, want %q", got, a.voiceURL(sess.ID()))
	}
	if got := form.Get("StatusCallback"); got != a.statusURL(sess.ID()) {
		t.Errorf("StatusCallback = %q", got)
	}
	if got := form.Get("Timeout"); got != "" {
		t.Errorf("Timeout = %q, want omitted by default", got)
	}
	if got := form.Get("Record"); got != "" {
		t.Errorf("Record = %q, want omitted (trial accounts reject it)", got)
	}
	if got := form.Get("Method"); got != "" {
		t.Errorf("Method = %q, want omitted (default POST)", got)
	}
	events := form["StatusCallbackEvent"]
	if len(events) != 4 {
		t.Errorf("StatusCallbackEvent = %v, want 4 values", events)
	}

	wantEvent(t, nextEvent(t, sess.Events()), domain.EventDialing, "")
}

func TestDialAPIError(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.failWith = &APIError{StatusCode: http.StatusBadRequest, Code: 21215}
	a := newTestAdapter(t, ts.URL)

	_, err := a.Dial(context.Background(), testOrder())
	if err == nil {
		t.Fatal("expected Dial error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 21215 {
		t.Fatalf("err = %v, want APIError 21215", err)
	}
	if strings.Contains(err.Error(), testAuthToken) {
		t.Fatal("error leaked the auth token")
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
	a := newTestAdapter(t, ts.URL)
	order := testOrder()
	order.Clinic.Phone = "0664 1234567"
	if _, err := a.Dial(context.Background(), order); err == nil {
		t.Fatal("expected invalid phone error")
	}
	f.mu.Lock()
	called := f.create != nil
	f.mu.Unlock()
	if called {
		t.Fatal("Twilio API must not be called for an invalid phone")
	}
}

func TestSendDTMFUpdatesCall(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL)

	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })
	s.progress(domain.EventAnswered)

	if err := s.SendDTMF(context.Background(), "1w2#"); err != nil {
		t.Fatalf("SendDTMF: %v", err)
	}
	f.mu.Lock()
	twiml := f.update.Get("Twiml")
	f.mu.Unlock()
	if !strings.Contains(twiml, `<Play digits="1w2#">`) || !strings.Contains(twiml, "<Connect>") {
		t.Fatalf("unexpected DTMF TwiML: %s", twiml)
	}
	if s.isEnded() {
		t.Fatal("DTMF redirect must not end the session")
	}
}

func TestSendDTMFBeforeAnswer(t *testing.T) {
	_, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() { _ = s.Hangup(context.Background()); drainEvents(s) })

	if err := s.SendDTMF(context.Background(), "1"); !errors.Is(err, ErrSessionNotReady) {
		t.Fatalf("SendDTMF before answer = %v, want ErrSessionNotReady", err)
	}
	if err := s.SendDTMF(context.Background(), "nope"); err == nil {
		t.Fatal("expected invalid digits error")
	}
}

func TestHangupEndsCallAndPostsTwilio(t *testing.T) {
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)

	if err := s.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if err := s.Hangup(context.Background()); err != nil {
		t.Fatalf("second Hangup: %v", err)
	}
	f.mu.Lock()
	end := f.end.Get("Status")
	f.mu.Unlock()
	if end != "completed" {
		t.Fatalf("end Status = %q, want completed", end)
	}
	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("SendAudio after end = %v, want ErrSessionEnded", err)
	}
	drainEvents(s)
}
