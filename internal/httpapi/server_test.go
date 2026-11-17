package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
)

const validOrderJSON = `{
  "clinic": {"name": "Praxis Test", "phone": "+431234567"},
  "patient": {
    "full_name": "Max Mustermann",
    "phone": "+436641234567",
    "birth_date": "1985-04-12",
    "reason": "Kontrolle",
    "preferred_slots": [{"start": "2026-11-20T09:00:00Z", "end": "2026-11-20T10:00:00Z"}]
  }
}`

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		HTTP: config.HTTP{Addr: "127.0.0.1:0"},
		Telephony: config.Telephony{
			Provider: "stub",
			Twilio:   config.Twilio{AccountSID: "secret-sid", AuthToken: "secret-token"},
		},
		STT:      config.STT{Provider: "stub"},
		TTS:      config.TTS{Provider: "stub"},
		Dialogue: config.Dialogue{Provider: "stub"},
		Storage:  config.Storage{Provider: "local", Local: config.Local{Dir: t.TempDir()}},
		Mistral:  config.Mistral{APIKey: "secret-key"},
		Log:      config.Log{Level: "error", Format: "text"},
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := testConfig(t)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ts := httptest.NewServer(New(cfg, a, "test-version").Handler)
	t.Cleanup(ts.Close)
	return ts
}

func doRequest(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeOrder(t *testing.T, resp *http.Response) domain.Order {
	t.Helper()
	var o domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	return o
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErr(t *testing.T, resp *http.Response) errEnvelope {
	t.Helper()
	var e errEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e
}

func createOrder(t *testing.T, ts *httptest.Server, body string) domain.Order {
	t.Helper()
	resp := doRequest(t, ts, http.MethodPost, "/api/v1/orders", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	return decodeOrder(t, resp)
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	resp := doRequest(t, ts, http.MethodGet, "/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, secret := range []string{"secret-sid", "secret-token", "secret-key"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("health response leaks %q: %s", secret, body)
		}
	}

	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.Version != "test-version" {
		t.Errorf("version = %q", got.Version)
	}
	if got.UptimeSeconds < 0 {
		t.Errorf("uptime = %d", got.UptimeSeconds)
	}
	for _, key := range []string{"telephony", "stt", "tts", "dialogue", "storage"} {
		if got.Providers[key] == "" {
			t.Errorf("providers[%q] is empty", key)
		}
	}
}

func TestCreateOrder(t *testing.T) {
	ts := newTestServer(t)
	resp := doRequest(t, ts, http.MethodPost, "/api/v1/orders", validOrderJSON)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	o := decodeOrder(t, resp)

	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(string(o.ID)) {
		t.Errorf("id = %q, want 32-hex", o.ID)
	}
	if o.Status != domain.OrderCreated {
		t.Errorf("status = %q, want created", o.Status)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}
	if o.Clinic.Name != "Praxis Test" || o.Patient.FullName != "Max Mustermann" {
		t.Errorf("fields not echoed: %+v", o)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/orders/"+string(o.ID) {
		t.Errorf("Location = %q", loc)
	}
}

func TestCreateOrderValidation(t *testing.T) {
	cases := map[string]struct {
		body       string
		wantStatus int
		wantCode   string
	}{
		"missing clinic name": {
			`{"clinic":{"phone":"+431"},"patient":{"full_name":"X"}}`,
			http.StatusBadRequest, "bad_request",
		},
		"missing clinic phone": {
			`{"clinic":{"name":"P"},"patient":{"full_name":"X"}}`,
			http.StatusBadRequest, "bad_request",
		},
		"missing patient name": {
			`{"clinic":{"name":"P","phone":"+431"},"patient":{}}`,
			http.StatusBadRequest, "bad_request",
		},
		"unknown field": {
			`{"clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"},"bogus":1}`,
			http.StatusBadRequest, "bad_request",
		},
		"client-set id": {
			`{"id":"0123456789abcdef0123456789abcdef","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`,
			http.StatusBadRequest, "bad_request",
		},
		"non-created status": {
			`{"status":"paid","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`,
			http.StatusBadRequest, "bad_request",
		},
		"bad birth date": {
			`{"clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X","birth_date":"12.04.1985"}}`,
			http.StatusBadRequest, "bad_request",
		},
		"bad slot range": {
			`{"clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X","preferred_slots":[{"start":"2026-11-20T10:00:00Z","end":"2026-11-20T09:00:00Z"}]}}`,
			http.StatusBadRequest, "bad_request",
		},
		"malformed json": {`{`, http.StatusBadRequest, "bad_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t)
			resp := doRequest(t, ts, http.MethodPost, "/api/v1/orders", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := decodeErr(t, resp).Error.Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

func TestCreateOrderReportsAllViolations(t *testing.T) {
	ts := newTestServer(t)
	resp := doRequest(t, ts, http.MethodPost, "/api/v1/orders", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	msg := decodeErr(t, resp).Error.Message
	for _, want := range []string{"clinic.name", "clinic.phone", "patient.full_name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

func TestCreateOrderExplicitCreatedStatus(t *testing.T) {
	ts := newTestServer(t)
	body := `{"status":"created","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`
	if got := createOrder(t, ts, body); got.Status != domain.OrderCreated {
		t.Errorf("status = %q", got.Status)
	}
}

func TestGetOrder(t *testing.T) {
	ts := newTestServer(t)
	created := createOrder(t, ts, validOrderJSON)

	resp := doRequest(t, ts, http.MethodGet, "/api/v1/orders/"+string(created.ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeOrder(t, resp)
	if got.ID != created.ID || got.Clinic.Name != created.Clinic.Name {
		t.Errorf("got %+v, want %+v", got, created)
	}
}

func TestGetOrderNotFound(t *testing.T) {
	ts := newTestServer(t)
	for _, id := range []string{string(domain.NewOrderID()), "not-an-id"} {
		resp := doRequest(t, ts, http.MethodGet, "/api/v1/orders/"+id, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", id, resp.StatusCode)
		}
		if code := decodeErr(t, resp).Error.Code; code != "not_found" {
			t.Errorf("%s: code = %q, want not_found", id, code)
		}
	}
}

func TestListOrders(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		ts := newTestServer(t)
		resp := doRequest(t, ts, http.MethodGet, "/api/v1/orders", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got := strings.TrimSpace(string(body)); got != "[]" {
			t.Errorf("body = %q, want []", got)
		}
	})

	t.Run("contains created order", func(t *testing.T) {
		ts := newTestServer(t)
		created := createOrder(t, ts, validOrderJSON)
		resp := doRequest(t, ts, http.MethodGet, "/api/v1/orders", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var list []domain.Order
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(list) != 1 || list[0].ID != created.ID {
			t.Fatalf("list = %+v, want the created order", list)
		}
	})
}

func TestBodyTooLarge(t *testing.T) {
	ts := newTestServer(t)
	huge := `{"clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"` + strings.Repeat("a", 2<<20) + `"}}`
	resp := doRequest(t, ts, http.MethodPost, "/api/v1/orders", huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "payload_too_large" {
		t.Errorf("code = %q", code)
	}
}

func TestWrongMethod(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/api/v1/orders", "/healthz"} {
		resp := doRequest(t, ts, http.MethodDelete, path, "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s: status = %d, want 405", path, resp.StatusCode)
		}
	}
}

func TestRequestID(t *testing.T) {
	ts := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-Id", "caller-supplied-id")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "caller-supplied-id" {
		t.Errorf("request id = %q, want caller-supplied-id", got)
	}

	resp2 := doRequest(t, ts, http.MethodGet, "/healthz", "")
	if resp2.Header.Get("X-Request-Id") == "" {
		t.Error("generated request id is empty")
	}
}

func TestRecovererConvertsPanic(t *testing.T) {
	cfg := testConfig(t)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	api := newAPI(cfg, a, "test")
	h := api.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var e errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Error.Code != "internal" || e.Error.Message != "internal error" {
		t.Errorf("envelope = %+v", e)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked to the client")
	}
}

func TestStatusRecorderUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec}
	if _, err := io.WriteString(sr, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if sr.status != http.StatusOK {
		t.Errorf("status = %d, want 200", sr.status)
	}
	sr.WriteHeader(http.StatusTeapot) // must be ignored: already wrote
	if sr.status != http.StatusOK {
		t.Errorf("status = %d after second WriteHeader, want 200", sr.status)
	}
	if sr.Unwrap() != rec {
		t.Error("Unwrap does not return the underlying writer")
	}
}

func TestTelephonyWebhooksMountedOnlyForTwilio(t *testing.T) {
	const callID = "0123456789abcdef0123456789abcdef"
	post := func(t *testing.T, ts *httptest.Server) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/telephony/twilio/status/"+callID, strings.NewReader("CallSid=CA123"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("post webhook: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("stub", func(t *testing.T) {
		if got := post(t, newTestServer(t)); got != http.StatusNotFound {
			t.Fatalf("stub webhook status = %d, want 404", got)
		}
	})

	t.Run("twilio", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.HTTP.PublicURL = "https://example.test"
		cfg.Telephony.Provider = "twilio"
		cfg.Telephony.Twilio = config.Twilio{
			AccountSID:         "AC" + strings.Repeat("0", 32),
			AuthToken:          "test-token",
			PhoneNumber:        "+15551234567",
			Mode:               "voice-api",
			RingTimeoutSeconds: 30,
		}
		a, err := app.New(cfg)
		if err != nil {
			t.Fatalf("app.New: %v", err)
		}
		ts := httptest.NewServer(New(cfg, a, "test").Handler)
		t.Cleanup(ts.Close)
		if got := post(t, ts); got != http.StatusForbidden {
			t.Fatalf("unsigned twilio status webhook = %d, want 403", got)
		}
	})
}

func TestWebSocketUpgradeThroughMiddleware(t *testing.T) {
	cfg := testConfig(t)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		_ = c.Write(context.Background(), websocket.MessageText, []byte("upgraded"))
	})
	a := &app.App{
		Config:   cfg,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Webhooks: wrapped,
	}
	ts := httptest.NewServer(New(cfg, a, "test").Handler)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/webhooks/telephony/twilio/stream/abc", nil)
	if err != nil {
		t.Fatalf("dial through middleware: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	_, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "upgraded" {
		t.Fatalf("message = %q", msg)
	}
}
