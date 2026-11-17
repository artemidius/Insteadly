// Package httpapi exposes the Insteadly JSON API. Handlers translate HTTP into calls
// on the core ports and map domain errors onto a stable JSON error envelope.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/web"
)

const (
	requestIDHeader = "X-Request-Id"
	sseHeartbeat    = 15 * time.Second
	sseRetryMS      = 3000
)

// API holds the handler dependencies. It is constructed once per process.
type API struct {
	cfg     config.Config
	app     *app.App
	version string
	started time.Time
	log     *slog.Logger

	// hub is the SSE source. Nil is tolerated (snapshot-only stream) so a bare test
	// App does not need an events hub.
	hub events.Subscriber
	// idem is the in-process place-call idempotency cache (prototype scope; C6 makes it
	// durable). TTL comes from api.idempotency_ttl.
	idem *idempotencyCache
	// heartbeat is the SSE comment interval; overridden by tests.
	heartbeat time.Duration
}

// New builds the HTTP server, including middleware. The listener address comes from
// cfg.HTTP.Addr; binding happens when the caller runs ListenAndServe.
func New(cfg config.Config, a *app.App, version string) *http.Server {
	api := newAPI(cfg, a, version)
	return &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.withMiddleware(api.routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// routes registers every route on a fresh mux. Tests use it directly so they can wrap
// the handlers with their own middleware/round-tripper.
func (a *API) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)

	// C6 auth surface. sessionAuth is a passthrough in auth.mode=off, where the handlers
	// themselves answer 501 so the C5 no-env API is unchanged.
	mux.HandleFunc("POST /api/v1/auth/register", a.register)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.sessionAuth(a.logout))
	mux.HandleFunc("GET /api/v1/auth/me", a.sessionAuth(a.me))

	// Order routes are scoped to the session user in cookie mode.
	mux.HandleFunc("GET /api/v1/orders", a.sessionAuth(a.listOrders))
	mux.HandleFunc("POST /api/v1/orders", a.sessionAuth(a.createOrder))
	mux.HandleFunc("GET /api/v1/orders/{id}", a.sessionAuth(a.getOrder))
	mux.HandleFunc("GET /api/v1/orders/{id}/events", a.sessionAuth(a.orderEvents))
	mux.HandleFunc("POST /api/v1/orders/{id}/checkout", a.sessionAuth(a.checkout))
	mux.HandleFunc("GET /api/v1/orders/{id}/checkout/simulate", a.sessionAuth(a.simulateCheckout))

	// C5 orchestration surface. Place-call is an eligibility endpoint, never a direct
	// dial: the scheduler picks the order up on its next tick. It stays API-key-gated and
	// is deliberately not the user payment path.
	mux.HandleFunc("POST /api/v1/orders/{id}/call", a.apiKeyAuth(a.callOrder))

	// Provider webhooks are mounted only when the selected adapter has them. They are
	// not part of the JSON API: Twilio posts form-encoded requests and expects TwiML.
	if a.app != nil && a.app.Webhooks != nil {
		// The provider handler serves POST callbacks and one GET WebSocket route; method
		// patterns keep Go's ServeMux from conflicting with the GET-only SPA fallback.
		mux.Handle("GET /webhooks/telephony/twilio/", a.app.Webhooks)
		mux.Handle("POST /webhooks/telephony/twilio/", a.app.Webhooks)
	}
	if a.paymentsProvider() == "stripe" {
		mux.HandleFunc("POST /webhooks/payments/stripe", a.stripeWebhook)
	}
	// An unmounted webhook path is 404, not the SPA and not a confusing 405.
	mux.HandleFunc("GET /webhooks/", notFound)
	mux.HandleFunc("POST /webhooks/", notFound)

	// The SPA is registered last and only for GET: /api/v1, /webhooks, and /healthz always
	// win, while a wrong method on an API route still gets 405 (not the SPA). Unknown
	// /api and /webhooks paths are 404, never index.html.
	mux.Handle("GET /", spaFallback())
	return mux
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
}

// spaFallback serves the SPA for browser routes but never masks an API/webhook miss.
func spaFallback() http.Handler {
	handler := web.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// withMiddleware wraps h with the shared middleware chain. Outermost first: recoverer ->
// requestID -> logger. A panic is still logged with its request id, and the access log
// line records the final status.
func (a *API) withMiddleware(h http.Handler) http.Handler {
	h = a.logRequests(h)
	h = requestID(h)
	h = a.recoverer(h)
	return h
}

// newAPI builds the handler set. New wraps it in middleware and a server; tests use
// it directly to exercise individual middlewares.
func newAPI(cfg config.Config, a *app.App, version string) *API {
	api := &API{
		cfg:       cfg,
		app:       a,
		version:   version,
		started:   time.Now(),
		log:       a.Log,
		idem:      newIdempotencyCache(cfg.API.IdempotencyTTL),
		heartbeat: sseHeartbeat,
	}
	if a.Events != nil {
		api.hub = a.Events
	}
	if cfg.API.Key == "" {
		api.log.Warn("place-call auth is disabled: INSTEADLY_API_KEY is not set (development only)")
	}
	return api
}

type healthResponse struct {
	Status        string            `json:"status"`
	Version       string            `json:"version"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Providers     map[string]string `json:"providers"`
}

// health reports liveness plus non-secret configuration, so ops can see which
// providers the process actually wired.
func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	notifyProvider := a.cfg.Notify.Provider
	if notifyProvider == "" {
		notifyProvider = "off" // zero Config == notifications disabled
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status:        "ok",
		Version:       a.version,
		UptimeSeconds: int64(time.Since(a.started).Seconds()),
		Providers: map[string]string{
			"telephony": a.cfg.Telephony.Provider,
			"stt":       a.cfg.STT.Provider,
			"tts":       a.cfg.TTS.Provider,
			"dialogue":  a.cfg.Dialogue.Provider,
			"storage":   a.cfg.Storage.Provider,
			"auth":      a.cfg.Auth.Mode,
			"payments":  a.cfg.Payments.Provider,
			"notify":    notifyProvider,
		},
	})
}

// Error envelope shared by every failure: {"error":{"code":...,"message":...}}.
type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		// Cannot happen for the response types in this package; keep the client
		// contract intact rather than writing a partial body.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: apiError{Code: code, Message: message}})
}

// writeStoreError maps storage failures onto the API contract. Unexpected errors are
// logged with detail but never leaked to the client.
func (a *API) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
	case errors.Is(err, ports.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource already exists")
	default:
		a.log.Error("request failed",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", requestIDFrom(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// --- middleware ---------------------------------------------------------------

type ctxKey int

const requestIDKey ctxKey = iota

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID honors an inbound X-Request-Id or generates one, then exposes it both in
// the response header and in the request context for logging.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// newRequestID is local on purpose: request IDs are an HTTP concern and must not depend
// on the domain ID format.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable in practice; a timestamp still lets
		// logs correlate this request.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// logRequests emits one structured access-log line per request.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestIDFrom(r.Context()),
		)
	})
}

// recoverer converts a handler panic into a logged 500 with the standard envelope.
func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.log.Error("panic in handler",
					"panic", fmt.Sprint(v),
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", requestIDFrom(r.Context()),
				)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log. Unwrap keeps it
// compatible with http.ResponseController (streaming endpoints arrive in C5).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
