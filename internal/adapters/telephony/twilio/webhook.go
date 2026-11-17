package twilio

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const maxWebhookBody = 64 << 10

func (a *Adapter) newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+voicePath+"{callID}", a.handleVoice)
	mux.HandleFunc("POST "+statusPath+"{callID}", a.handleStatus)
	mux.HandleFunc("POST "+streamStatusPath+"{callID}", a.handleStreamStatus)
	mux.HandleFunc("GET "+streamPath+"{callID}", a.handleStream)
	return mux
}

func (a *Adapter) handleVoice(w http.ResponseWriter, r *http.Request) {
	id := domain.CallID(r.PathValue("callID"))
	if !a.parseForm(w, r) {
		return
	}
	s := a.lookup(id)
	if s == nil {
		http.NotFound(w, r)
		return
	}
	if !a.authorizeVoice(r, s) {
		a.log.Warn("twilio: rejected voice request",
			"path", r.URL.Path,
			"request_id", requestID(w),
		)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var body []byte
	var err error
	if s.isEnded() {
		body, err = a.hangupTwiML()
	} else {
		body, err = a.voiceTwiML(id)
	}
	if err != nil {
		a.log.Error("twilio: build twiml failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.log.Debug("twilio: serving voice TwiML", "call_id", id, "twiml", string(body))
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (a *Adapter) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := domain.CallID(r.PathValue("callID"))
	if !a.authorize(w, r) {
		return
	}
	status := r.PostForm.Get("CallStatus")
	s := a.lookup(id)
	if s == nil {
		// Unknown or already-finished calls are acknowledged so Twilio stops retrying.
		a.log.Debug("twilio: status for unknown call", "status", status, "request_id", requestID(w))
		w.WriteHeader(http.StatusOK)
		return
	}
	s.log.Debug("twilio: status callback", "status", status, "request_id", requestID(w))
	s.handleStatus(status)
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	// Diagnostics only: log whatever Twilio reports about stream setup, even if the
	// request is unsigned (the TwiML fetch has shown the same behaviour). No state is
	// changed here, and nothing is returned but 200.
	if !a.parseForm(w, r) {
		return
	}
	if sig := r.Header.Get("X-Twilio-Signature"); sig != "" &&
		!validSignature(a.cfg.AuthToken, signedURL(r, a.publicBaseURL), sig, r.PostForm) {
		a.log.Warn("twilio: stream status signature mismatch", "request_id", requestID(w))
	}
	event := r.PostForm.Get("StreamEvent")
	logFn := a.log.Info
	if event == "stream-error" {
		logFn = a.log.Warn
	}
	logFn("twilio: stream status",
		"event", event,
		"error", r.PostForm.Get("StreamError"),
		"stream_sid", r.PostForm.Get("StreamSid"),
		"call_sid", r.PostForm.Get("CallSid"),
		"request_id", requestID(w),
	)
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) handleStream(w http.ResponseWriter, r *http.Request) {
	id := domain.CallID(r.PathValue("callID"))
	s := a.lookup(id)
	if s == nil || s.isEnded() {
		http.NotFound(w, r)
		return
	}
	a.serveStream(w, r, s)
}

// parseForm bounds and parses a Twilio form body.
func (a *Adapter) parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return false
	}
	return true
}

// authorizeVoice protects the TwiML fetch. Twilio does not sign the Url fetch for an
// outbound call (observed live 2026-11-17: no X-Twilio-Signature header on the voice
// request, while status callbacks are signed), so the unguessable call ID plus the
// AccountSid/CallSid form fields are the capability check. If a signature is present it
// must still be valid.
func (a *Adapter) authorizeVoice(r *http.Request, s *session) bool {
	if got := r.PostForm.Get("AccountSid"); got != a.cfg.AccountSID {
		return false
	}
	sid := s.providerSID()
	if sid == "" || r.PostForm.Get("CallSid") != sid {
		return false
	}
	if signature := r.Header.Get("X-Twilio-Signature"); signature != "" {
		return validSignature(a.cfg.AuthToken, signedURL(r, a.publicBaseURL), signature, r.PostForm)
	}
	a.log.Debug("twilio: unsigned TwiML fetch accepted for known call", "call_id", s.id)
	return true
}

// authorize enforces Twilio's webhook signature on status callbacks. It parses the form
// and is called before any session lookup.
func (a *Adapter) authorize(w http.ResponseWriter, r *http.Request) bool {
	if !a.parseForm(w, r) {
		return false
	}
	signature := r.Header.Get("X-Twilio-Signature")
	signed := signedURL(r, a.publicBaseURL)
	if !validSignature(a.cfg.AuthToken, signed, signature, r.PostForm) {
		// Signature failures are hard to diagnose from the outside (tunnel URL mismatch,
		// proxy rewriting, Twilio adding parameters). Log the signed URL, parameter names,
		// and both HMACs; none of those contain the auth token or form values.
		names := make([]string, 0, len(r.PostForm))
		for k := range r.PostForm {
			names = append(names, k)
		}
		sort.Strings(names)
		a.log.Warn("twilio: rejected webhook signature",
			"path", r.URL.Path,
			"signed_url", signed,
			"public_base", a.publicBaseURL,
			"host", r.Host,
			"request_uri", r.RequestURI,
			"content_type", r.Header.Get("Content-Type"),
			"param_names", strings.Join(names, ","),
			"param_count", len(r.PostForm),
			"provided_signature", signature,
			"expected_signature", signatureFor(a.cfg.AuthToken, signed),
			"request_id", requestID(w),
		)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if strings.TrimSpace(r.Header.Get("X-Twilio-Webhooks-Signature-Settings")) != "" {
		// Account-auth-token webhooks are HMAC-SHA1. A shared-key SHA256 webhook is not
		// something this adapter is configured for; fail closed instead of guessing.
		a.log.Warn("twilio: unsupported shared-key signature settings")
		http.Error(w, "unsupported signature algorithm", http.StatusForbidden)
		return false
	}
	return true
}

// requestID reads the response header set by httpapi's middleware without importing it.
func requestID(w http.ResponseWriter) string {
	return w.Header().Get("X-Request-Id")
}
