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
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const requestIDHeader = "X-Request-Id"

// API holds the handler dependencies. It is constructed once per process.
type API struct {
	cfg     config.Config
	app     *app.App
	version string
	started time.Time
	log     *slog.Logger
}

// New builds the HTTP server, including middleware. The listener address comes from
// cfg.HTTP.Addr; binding happens when the caller runs ListenAndServe.
func New(cfg config.Config, a *app.App, version string) *http.Server {
	api := newAPI(cfg, a, version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /api/v1/orders", api.listOrders)
	mux.HandleFunc("POST /api/v1/orders", api.createOrder)
	mux.HandleFunc("GET /api/v1/orders/{id}", api.getOrder)

	// Outermost first: recoverer -> requestID -> logger. A panic is still logged with
	// its request id, and the access log line records the final status.
	var handler http.Handler = mux
	handler = api.logRequests(handler)
	handler = requestID(handler)
	handler = api.recoverer(handler)

	return &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// newAPI builds the handler set. New wraps it in middleware and a server; tests use
// it directly to exercise individual middlewares.
func newAPI(cfg config.Config, a *app.App, version string) *API {
	return &API{
		cfg:     cfg,
		app:     a,
		version: version,
		started: time.Now(),
		log:     a.Log,
	}
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
