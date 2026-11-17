package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
)

// cookieAuthConfig is the standard C6 web-app configuration for HTTP tests: cookie auth
// with the cheapest bcrypt cost and stub payments.
func cookieAuthConfig(c *config.Config) {
	c.Auth = config.Auth{
		Mode:       "cookie",
		SessionTTL: time.Hour,
		CookieName: "insteadly_session",
		BcryptCost: 4,
	}
	c.Payments = config.Payments{Provider: "stub"}
}

func newCookieServer(t *testing.T) (*httptest.Server, *app.App) {
	t.Helper()
	return newTestServerWithApp(t, cookieAuthConfig)
}

func doWithCookie(t *testing.T, ts *httptest.Server, method, path, body string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func findCookie(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func registerUser(t *testing.T, ts *httptest.Server, email, password string) (*http.Cookie, domain.User) {
	t.Helper()
	return registerUserWithCookieName(t, ts, "insteadly_session", email, password)
}

func registerUserWithCookieName(t *testing.T, ts *httptest.Server, cookieName, email, password string) (*http.Cookie, domain.User) {
	t.Helper()
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}
	cookie := findCookie(t, resp, cookieName)
	if cookie == nil {
		t.Fatalf("register did not set the %q session cookie", cookieName)
	}
	var body userEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return cookie, body.User
}

func TestRegisterSetsSecureCookieAttributes(t *testing.T) {
	ts, _ := newCookieServer(t)
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"  User@Example.TEST ","password":"correct horse battery"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	cookie := findCookie(t, resp, "insteadly_session")
	if cookie == nil {
		t.Fatal("Set-Cookie missing")
	}
	if cookie.Value == "" {
		t.Error("cookie value is empty")
	}
	if !cookie.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge <= 0 || cookie.MaxAge > int(time.Hour.Seconds()) {
		t.Errorf("MaxAge = %d, want about 1h", cookie.MaxAge)
	}
	if cookie.Secure {
		t.Error("cookie is Secure over http://127.0.0.1")
	}

	var body userEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.User.Email != "user@example.test" {
		t.Errorf("email = %q, want normalized lowercase", body.User.Email)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(string(body.User.ID)) {
		t.Errorf("user id = %q", body.User.ID)
	}
	if strings.Contains(resp.Header.Get("Set-Cookie"), "password") {
		t.Error("Set-Cookie leaks the password")
	}
}

func TestRegisterValidation(t *testing.T) {
	ts, _ := newCookieServer(t)
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"bogus","password":"short"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	msg := decodeErr(t, resp).Error.Message
	for _, want := range []string{"name@example.com", "at least 8"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}

	resp = doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"ok@example.test","password":"password123","bogus":1}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", resp.StatusCode)
	}
}

func TestDuplicateRegistrationConflict(t *testing.T) {
	ts, _ := newCookieServer(t)
	registerUser(t, ts, "dup@example.test", "password123")
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"DUP@example.test","password":"password456"}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}
}

func TestLoginLogoutMeFlow(t *testing.T) {
	ts, _ := newCookieServer(t)
	cookie, _ := registerUser(t, ts, "flow@example.test", "password123")

	// me with the registration cookie works.
	resp := doWithCookie(t, ts, http.MethodGet, "/api/v1/auth/me", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, want 200", resp.StatusCode)
	}
	var me userEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.User.Email != "flow@example.test" {
		t.Errorf("me email = %q", me.User.Email)
	}
}

func TestMeUnauthenticated(t *testing.T) {
	ts, _ := newCookieServer(t)
	resp := doWithCookie(t, ts, http.MethodGet, "/api/v1/auth/me", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "unauthorized" {
		t.Errorf("code = %q", code)
	}
}

func TestLoginRejectsBadCredentialsWithoutOracle(t *testing.T) {
	ts, _ := newCookieServer(t)
	registerUser(t, ts, "oracle@example.test", "password123")

	cases := []string{
		`{"email":"nobody@example.test","password":"password123"}`,
		`{"email":"oracle@example.test","password":"wrong-password"}`,
	}
	var messages []string
	for _, body := range cases {
		resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/login", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		envelope := decodeErr(t, resp)
		if envelope.Error.Code != "unauthorized" {
			t.Errorf("code = %q", envelope.Error.Code)
		}
		messages = append(messages, envelope.Error.Message)
	}
	if messages[0] != messages[1] {
		t.Errorf("login errors differ (%q vs %q); this leaks account existence", messages[0], messages[1])
	}
}

func TestLogoutRevokesAndClearsCookie(t *testing.T) {
	ts, _ := newCookieServer(t)
	cookie, _ := registerUser(t, ts, "logout@example.test", "password123")

	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/logout", "", cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	cleared := findCookie(t, resp, "insteadly_session")
	if cleared == nil || cleared.Value != "" {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}

	resp = doWithCookie(t, ts, http.MethodGet, "/api/v1/auth/me", "", cookie)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout status = %d, want 401 (session revoked)", resp.StatusCode)
	}
}

func TestAuthRoutesReturn501WhenOff(t *testing.T) {
	ts, _ := newTestServerWithApp(t, nil) // auth.mode defaults to off
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/auth/register", `{"email":"a@example.test","password":"password123"}`},
		{http.MethodPost, "/api/v1/auth/login", `{"email":"a@example.test","password":"password123"}`},
		{http.MethodPost, "/api/v1/auth/logout", ""},
		{http.MethodGet, "/api/v1/auth/me", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := doWithCookie(t, ts, tc.method, tc.path, tc.body, nil)
			if resp.StatusCode != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501", resp.StatusCode)
			}
			if code := decodeErr(t, resp).Error.Code; code != "not_implemented" {
				t.Errorf("code = %q, want not_implemented", code)
			}
		})
	}

	// C5's unauthenticated order API still works with auth off.
	created := createOrder(t, ts, validOrderJSON)
	if created.UserID != "" {
		t.Errorf("auth-off order has user_id %q, want empty", created.UserID)
	}
	checkout := doWithCookie(t, ts, http.MethodPost, "/api/v1/orders/"+string(created.ID)+"/checkout", "", nil)
	if checkout.StatusCode != http.StatusNotImplemented {
		t.Errorf("checkout auth-off status = %d, want 501", checkout.StatusCode)
	}
	simulate := doWithCookie(t, ts, http.MethodGet, "/api/v1/orders/"+string(created.ID)+"/checkout/simulate", "", nil)
	if simulate.StatusCode != http.StatusNotImplemented {
		t.Errorf("simulate auth-off status = %d, want 501", simulate.StatusCode)
	}
}

func TestSecureCookieWhenPublicURLIsHTTPS(t *testing.T) {
	ts, _ := newTestServerWithApp(t, func(c *config.Config) {
		cookieAuthConfig(c)
		c.HTTP.PublicURL = "https://insteadly.example.test"
	})
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/auth/register",
		`{"email":"secure@example.test","password":"password123"}`, nil)
	cookie := findCookie(t, resp, "insteadly_session")
	if cookie == nil || !cookie.Secure {
		t.Fatalf("cookie = %+v, want Secure over https", cookie)
	}
}

func TestCookieNameFromConfig(t *testing.T) {
	ts, _ := newTestServerWithApp(t, func(c *config.Config) {
		cookieAuthConfig(c)
		c.Auth.CookieName = "custom_session"
	})
	cookie, _ := registerUserWithCookieName(t, ts, "custom_session", "custom@example.test", "password123")
	if cookie.Name != "custom_session" {
		t.Errorf("cookie name = %q, want custom_session", cookie.Name)
	}
	resp := doWithCookie(t, ts, http.MethodGet, "/api/v1/auth/me", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("me with custom cookie status = %d, want 200", resp.StatusCode)
	}
}
