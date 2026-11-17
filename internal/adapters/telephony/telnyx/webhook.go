package telnyx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const maxWebhookBody = 64 << 10

// webhookEvent is the verified Telnyx call-control webhook envelope:
//
//	{"data":{"event_type":"call.answered","id":"…","occurred_at":"…","payload":{…}}}
type webhookEvent struct {
	Data struct {
		EventType  string         `json:"event_type"`
		ID         string         `json:"id"`
		OccurredAt string         `json:"occurred_at"`
		Payload    webhookPayload `json:"payload"`
	} `json:"data"`
}

type webhookPayload struct {
	CallControlID string `json:"call_control_id"`
	ConnectionID  string `json:"connection_id"`
	CallLegID     string `json:"call_leg_id"`
	CallSessionID string `json:"call_session_id"`
	Direction     string `json:"direction"`
	State         string `json:"state"`
	HangupCause   string `json:"hangup_cause"`
	HangupSource  string `json:"hangup_source"`
	StreamID      string `json:"stream_id"`
}

func (a *Adapter) newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+webhookPath+"{callID}", a.handleWebhook)
	mux.HandleFunc("GET "+streamPath+"{callID}", a.handleStream)
	return mux
}

// handleWebhook accepts one call-control event. Authentication is layered: when the
// Ed25519 signature headers are present they must verify (fail closed, including when no
// public key is configured); the unguessable per-call path plus the payload's
// call_control_id/connection_id match is the capability check for unsigned callbacks.
func (a *Adapter) handleWebhook(w http.ResponseWriter, r *http.Request) {
	id := domain.CallID(r.PathValue("callID"))
	if id == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.verifySignedRequest(r.Header, body); err != nil {
		a.log.Warn("telnyx: rejected webhook signature", "call_id", id, "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	eventType := event.Data.EventType
	if eventType == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s := a.lookup(id)
	if s == nil {
		// Unknown or already-finished calls are acknowledged so Telnyx stops retrying.
		a.log.Debug("telnyx: webhook for unknown call", "event", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !a.authorizeWebhook(s, event, eventType) {
		a.log.Warn("telnyx: rejected webhook capability check", "call_id", id, "event", eventType)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	s.log.Debug("telnyx: webhook event", "event", eventType)
	s.handleWebhookEvent(eventType, event.Data.Payload)
	w.WriteHeader(http.StatusOK)
}

// handleStream upgrades an authenticated media-stream request. An unknown or finished
// call is a 404 so a stale stream_url does not linger.
func (a *Adapter) handleStream(w http.ResponseWriter, r *http.Request) {
	id := domain.CallID(r.PathValue("callID"))
	s := a.lookup(id)
	if s == nil || s.isEnded() {
		http.NotFound(w, r)
		return
	}
	a.serveStream(w, r, s)
}

// authorizeWebhook enforces the capability check. The path call id is unguessable; the
// payload must corroborate the call (call_control_id) and the configured Call Control
// application (connection_id, when the event carries one).
func (a *Adapter) authorizeWebhook(s *session, event webhookEvent, eventType string) bool {
	p := event.Data.Payload
	if p.CallControlID == "" {
		return false
	}
	if p.ConnectionID != "" && p.ConnectionID != a.cfg.ConnectionID {
		return false
	}
	s.mu.Lock()
	mismatch := s.ccid != "" && s.ccid != p.CallControlID
	if s.ccid == "" && !mismatch {
		// The Dial response and the first webhook can race; the unguessable path plus a
		// matching connection id make first-writer-wins safe.
		s.ccid = p.CallControlID
	}
	s.mu.Unlock()
	if mismatch {
		return false
	}
	// call_control_id and connection_id are transport ids, never logged with values.
	_ = eventType
	return true
}

// handleWebhookEvent normalizes one verified call-control event onto the session state
// machine. Event names and payload fields stay inside the adapter.
func (s *session) handleWebhookEvent(eventType string, p webhookPayload) {
	switch eventType {
	case "call.initiated":
		if p.Direction == "incoming" {
			// This adapter dials outbound calls only; hang up an unexpected inbound call
			// rather than driving it through the runner.
			s.log.Warn("telnyx: rejecting unexpected inbound call")
			go func() { _ = s.Hangup(context.Background()) }()
			return
		}
		s.progress(domain.EventDialing)
	case "call.answered":
		s.progress(domain.EventAnswered)
		s.startStream()
	case "call.hangup":
		s.finish(hangupReason(p.HangupCause))
	case "streaming.started", "streaming.stopped":
		s.log.Debug("telnyx: streaming lifecycle event", "event", eventType, "stream_id", p.StreamID)
	default:
		s.log.Debug("telnyx: ignoring call-control event", "event", eventType)
	}
}

func hangupReason(cause string) domain.EndReason {
	if reason, ok := hangupCauses[cause]; ok {
		return reason
	}
	return domain.EndError
}
