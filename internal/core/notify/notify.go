// Package notify turns terminal order outcomes into outbound notifications. It is core:
// it imports domain, ports, and events — never an adapter.
//
// The worker is deliberately best-effort and in-process: it subscribes to the existing
// order-status hub, sends one notification per finished order event, and logs (never
// retries infinitely) a delivery failure. A durable outbox with retry is a later seam.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	// sendTimeout bounds one delivery so a stalled notifier cannot wedge the worker.
	sendTimeout = 15 * time.Second
	// maxDedupe bounds the (order, attempt) memory. A notification is only ever sent for
	// a terminal event, so old keys are unlikely to repeat; FIFO eviction keeps a
	// long-running process bounded.
	maxDedupe = 2048
)

// Config wires the worker. Users is optional (nil = no email resolution; the recipient
// falls back to the patient phone). Notifier must not be nil.
type Config struct {
	Events   events.Subscriber
	Orders   ports.OrderStore
	Users    ports.AuthStore // optional
	Notifier ports.Notifier
	Logger   *slog.Logger
}

// Worker subscribes to the order-status hub and sends one notification for every finished
// order event (Status == finished, Outcome != nil), deduped by (OrderID, Attempts).
type Worker struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	seen  map[dedupeKey]struct{}
	order []dedupeKey

	// started is a test-only readiness signal closed once the hub subscription is live.
	started chan struct{}
}

type dedupeKey struct {
	id       domain.OrderID
	attempts int
}

// New validates the wiring and performs no I/O.
func New(cfg Config) (*Worker, error) {
	if cfg.Events == nil {
		return nil, errors.New("notify: events subscriber is required")
	}
	if cfg.Orders == nil {
		return nil, errors.New("notify: order store is required")
	}
	if cfg.Notifier == nil {
		return nil, errors.New("notify: notifier is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Worker{
		cfg:  cfg,
		log:  cfg.Logger,
		seen: make(map[dedupeKey]struct{}),
	}, nil
}

// Run blocks until ctx is canceled or the hub closes the subscription. It is the worker's
// main loop; start it from the composition root. Send is best-effort: a Send error is
// logged, never retried infinitely (a durable outbox is a later concern).
func (w *Worker) Run(ctx context.Context) error {
	ch, unsubscribe, err := w.cfg.Events.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("notify: subscribe: %w", err)
	}
	if unsubscribe != nil {
		defer unsubscribe()
	}
	if w.started != nil {
		close(w.started)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			w.handle(ctx, ev)
		}
	}
}

// handle processes exactly one hub event. Only a terminal finished event with an outcome
// is actionable; everything else is ignored.
func (w *Worker) handle(ctx context.Context, ev events.OrderEvent) {
	if ev.Status != domain.OrderFinished || ev.Outcome == nil {
		return
	}
	key := dedupeKey{id: ev.OrderID, attempts: ev.Attempts}
	if !w.firstSighting(key) {
		w.log.Debug("notify: duplicate terminal event ignored", "order_id", ev.OrderID, "attempts", ev.Attempts)
		return
	}

	order, err := w.cfg.Orders.Get(ctx, ev.OrderID)
	if err != nil {
		w.log.Warn("notify: load order failed", "order_id", ev.OrderID, "error", err)
		return
	}
	notification := w.notification(ctx, order, ev.Outcome)

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := w.cfg.Notifier.Send(sendCtx, notification); err != nil && ctx.Err() == nil {
		w.log.Warn("notify: send failed",
			"order_id", notification.OrderID,
			"channel", notification.Channel,
			"error", err,
		)
	}
}

// firstSighting records one (order, attempt) key and reports whether it is new.
func (w *Worker) firstSighting(key dedupeKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[key]; ok {
		return false
	}
	w.seen[key] = struct{}{}
	w.order = append(w.order, key)
	if len(w.order) > maxDedupe {
		oldest := w.order[0]
		w.order = w.order[1:]
		delete(w.seen, oldest)
	}
	return true
}

// notification builds the fixed, non-secret message. Recipient resolution prefers the
// owning account's email, then the patient phone, then an empty email recipient.
func (w *Worker) notification(ctx context.Context, order domain.Order, outcome *domain.Outcome) domain.Notification {
	channel, to := w.recipient(ctx, order)
	subject, body := summarize(outcome)
	return domain.Notification{
		OrderID: order.ID,
		Channel: channel,
		To:      to,
		Subject: subject,
		Body:    body,
	}
}

func (w *Worker) recipient(ctx context.Context, order domain.Order) (channel, to string) {
	if order.UserID != "" && w.cfg.Users != nil {
		user, err := w.cfg.Users.GetUser(ctx, order.UserID)
		switch {
		case err == nil && strings.TrimSpace(user.Email) != "":
			return "email", user.Email
		case err != nil:
			w.log.Debug("notify: user lookup failed; falling back", "order_id", order.ID, "error", err)
		}
	}
	if phone := strings.TrimSpace(order.Patient.Phone); phone != "" {
		return "sms", phone
	}
	return "email", ""
}

// summarize maps the outcome to a fixed subject/body. It never includes patient details,
// payment data, or free-form conversation text.
func summarize(outcome *domain.Outcome) (subject, body string) {
	if outcome == nil {
		return "Anruf abgeschlossen", "Der Anruf wurde abgeschlossen."
	}
	switch outcome.Kind {
	case domain.OutcomeBooked:
		subject = "Termin bestätigt"
		body = "Ihr Termin wurde bestätigt."
		if slot := outcome.BookedSlot; slot != nil {
			body += " Termin: " + slot.DateTime.Format("02.01.2006 15:04") + "."
			if slot.Doctor != "" {
				body += " Arzt: " + slot.Doctor + "."
			}
			if slot.Practice != "" {
				body += " Praxis: " + slot.Practice + "."
			}
		}
	case domain.OutcomeCallbackNeeded:
		subject = "Rückruf erforderlich"
		body = "Die Praxis bittet um einen Rückruf."
		if outcome.CallbackAt != nil {
			body += " Rückrufzeit: " + outcome.CallbackAt.Format("02.01.2006 15:04") + "."
		}
	case domain.OutcomeFailed:
		subject = "Anruf fehlgeschlagen"
		body = "Der Anruf konnte nicht erfolgreich abgeschlossen werden."
		if outcome.FailReason != "" {
			body += " Grund: " + outcome.FailReason + "."
		}
	case domain.OutcomeAborted:
		subject = "Anruf abgebrochen"
		body = "Der Anruf wurde abgebrochen."
	default:
		subject = "Anruf abgeschlossen"
		body = "Der Anruf wurde abgeschlossen."
	}
	return subject, body
}
