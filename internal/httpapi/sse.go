package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
)

// orderEvents streams one order's status changes as Server-Sent Events. It sends a full
// order snapshot first, then hub events for that order ID. Last-Event-ID is accepted but
// deliberately ignored: there is no replay buffer in C5 because every (re)connect gets a
// fresh snapshot, which makes replay redundant. A slow client's hub buffer overflows and
// closes; the stream then ends with event:error/code=overrun so the client reconnects and
// re-snapshots.
func (a *API) orderEvents(w http.ResponseWriter, r *http.Request) {
	id := domain.OrderID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "order id is required")
		return
	}
	order, err := a.app.Storage.Orders().Get(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	if !a.ownsOrder(r, order) {
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
		return
	}

	// Subscribe before the snapshot so an event published after the read is buffered
	// rather than lost. A closed hub degrades to a snapshot-only stream.
	var (
		ch    <-chan events.OrderEvent
		unsub func()
	)
	if a.hub != nil {
		if subscribed, unsubscribe, err := a.hub.Subscribe(r.Context()); err == nil {
			ch, unsub = subscribed, unsubscribe
			defer unsub()
		}
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetryMS); err != nil {
		return
	}
	if err := writeSSE(w, rc, "order", order); err != nil {
		return
	}
	if ch == nil {
		return
	}

	heartbeat := a.heartbeat
	if heartbeat <= 0 {
		heartbeat = sseHeartbeat
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Overrun (slow subscriber) or hub shutdown. Both mean "reconnect and
				// re-snapshot"; overrun is the documented client-visible code.
				_ = writeSSE(w, rc, "error", map[string]string{"code": "overrun"})
				return
			}
			if ev.OrderID != id {
				continue
			}
			if err := writeSSE(w, rc, "order", ev); err != nil {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// writeSSE frames one event and flushes it immediately.
func writeSSE(w io.Writer, rc *http.ResponseController, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	return rc.Flush()
}
