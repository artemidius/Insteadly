package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/artemidius/insteadly/internal/core/auth"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	maxAuthBodyBytes = 16 << 10
	defaultCookieTTL = 24 * time.Hour
	fallbackCookie   = "insteadly_session"
)

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userEnvelope struct {
	User domain.User `json:"user"`
}

// register creates an account and opens a session. Available only in cookie mode.
func (a *API) register(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}
	user, token, err := a.app.Auth.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		var validation *coreauth.ValidationError
		switch {
		case errors.As(err, &validation):
			writeError(w, http.StatusBadRequest, "bad_request", validation.Error())
		case errors.Is(err, ports.ErrConflict):
			writeError(w, http.StatusConflict, "conflict", "email is already registered")
		default:
			a.writeAuthError(w, r, err)
		}
		return
	}
	a.setSessionCookie(w, token)
	writeJSON(w, http.StatusCreated, userEnvelope{User: user})
}

// login verifies credentials and opens a session. Unknown email and wrong password are
// deliberately indistinguishable.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}
	user, token, err := a.app.Auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, coreauth.ErrBadCredentials) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid email or password")
			return
		}
		a.writeAuthError(w, r, err)
		return
	}
	a.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, userEnvelope{User: user})
}

// logout revokes the current session and clears the cookie. It is idempotent.
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	if err := a.app.Auth.Logout(r.Context(), sessionToken(r, a.cookieName())); err != nil {
		a.writeAuthError(w, r, err)
		return
	}
	a.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// me bootstraps the SPA session; it is wrapped by sessionAuth.
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid session is required")
		return
	}
	writeJSON(w, http.StatusOK, userEnvelope{User: user})
}

// sessionAuth wraps user-facing order routes and /me/logout. In auth.mode=off it is a
// passthrough, preserving the C5 unauthenticated API; in cookie mode it resolves the
// cookie to a user and stores it in the request context.
func (a *API) sessionAuth(next http.HandlerFunc) http.HandlerFunc {
	if !a.authCookieMode() {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if a.app.Auth == nil {
			writeError(w, http.StatusNotImplemented, "not_implemented",
				"user auth is disabled; set INSTEADLY_AUTH_MODE=cookie")
			return
		}
		user, err := a.app.Auth.Authenticate(r.Context(), sessionToken(r, a.cookieName()))
		if err != nil {
			if errors.Is(err, coreauth.ErrUnauthenticated) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "a valid session is required")
				return
			}
			a.writeAuthError(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

// requireAuthConfigured rejects user-auth routes when cookie auth is off.
func (a *API) requireAuthConfigured(w http.ResponseWriter) bool {
	if a.authCookieMode() && a.app.Auth != nil {
		return true
	}
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"user auth is disabled; set INSTEADLY_AUTH_MODE=cookie")
	return false
}

func (a *API) authCookieMode() bool { return a.cfg.Auth.Mode == "cookie" }

func (a *API) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("auth request failed",
		"error", err,
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", requestIDFrom(r.Context()),
	)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func decodeCredentials(w http.ResponseWriter, r *http.Request, dst *credentialsRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 16 KiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// userKey is the context key for the authenticated user.
type userCtxKey struct{}

var userKey userCtxKey

func currentUser(r *http.Request) (domain.User, bool) {
	user, ok := r.Context().Value(userKey).(domain.User)
	return user, ok
}

func sessionToken(r *http.Request, cookieName string) string {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func (a *API) cookieName() string {
	if name := strings.TrimSpace(a.cfg.Auth.CookieName); name != "" {
		return name
	}
	return fallbackCookie
}

func (a *API) sessionTTL() time.Duration {
	if a.app.Auth != nil {
		return a.app.Auth.SessionTTL()
	}
	if ttl := a.cfg.Auth.SessionTTL; ttl > 0 {
		return ttl
	}
	return defaultCookieTTL
}

// secureCookies tracks whether the browser reaches this process over https. Dev over
// http://127.0.0.1 must still be able to set the cookie.
func (a *API) secureCookies() bool {
	return strings.HasPrefix(a.cfg.HTTP.PublicURL, "https://")
}

func (a *API) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(a.sessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *API) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// ownsOrder enforces user scoping in cookie mode. Auth-off mode keeps the C5 behavior.
func (a *API) ownsOrder(r *http.Request, order domain.Order) bool {
	if !a.authCookieMode() {
		return true
	}
	user, ok := currentUser(r)
	if !ok {
		return false
	}
	return order.UserID != "" && order.UserID == user.ID
}
