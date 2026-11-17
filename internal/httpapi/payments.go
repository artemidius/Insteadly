package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// maxWebhookBodyBytes bounds the raw provider payload (Stripe events are small).
const maxWebhookBodyBytes = 64 << 10

// checkout starts a payment for one owned order and returns the provider redirect URL.
func (a *API) checkout(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	order, ok := a.ownedOrder(w, r)
	if !ok {
		return
	}
	if a.app.Payments == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "payments are not configured")
		return
	}
	redirect, err := a.app.Payments.CreateCheckout(r.Context(), order)
	if err != nil {
		a.log.Error("create checkout failed",
			"error", err,
			"order_id", order.ID,
			"request_id", requestIDFrom(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal", "could not start checkout")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect})
}

// simulateCheckout is the stub payments path: an authenticated GET that drives the same
// domain.MarkPaid seam the Stripe webhook uses, then redirects to the dashboard.
func (a *API) simulateCheckout(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthConfigured(w) {
		return
	}
	if a.paymentsProvider() != "stub" {
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
		return
	}
	order, ok := a.ownedOrder(w, r)
	if !ok {
		return
	}
	if _, err := a.app.Storage.Orders().Update(r.Context(), order.ID, func(o *domain.Order) error {
		return o.MarkPaid("stub/"+string(order.ID), time.Now())
	}); err != nil {
		a.writeStoreError(w, r, err)
		return
	}
	location := "/dashboard?order=" + url.QueryEscape(string(order.ID))
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// stripeWebhook verifies a signed Stripe event and applies checkout.session.completed to
// the order. A bad signature is a 400 (never retried); an unknown order is acknowledged
// so Stripe stops redelivering it.
func (a *API) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	if a.app.Payments == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "payments are not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 64 KiB")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read webhook body")
		return
	}
	signature := r.Header.Get("Stripe-Signature")
	event, err := a.app.Payments.HandleWebhook(r.Context(), body, signature)
	if err != nil {
		a.log.Warn("payment webhook rejected",
			"error", err,
			"request_id", requestIDFrom(r.Context()),
		)
		writeError(w, http.StatusBadRequest, "bad_request", "invalid webhook signature")
		return
	}
	if !event.Paid || event.OrderID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	reference := event.Reference
	if reference == "" {
		reference = string(event.OrderID)
	}
	if _, err := a.app.Storage.Orders().Update(r.Context(), event.OrderID, func(o *domain.Order) error {
		return o.MarkPaid(reference, event.At)
	}); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			// Payment for an order this instance does not know about: ack and move on.
			a.log.Info("payment webhook for unknown order",
				"order_id", event.OrderID,
				"reference", reference,
				"request_id", requestIDFrom(r.Context()),
			)
			w.WriteHeader(http.StatusOK)
			return
		}
		a.log.Error("payment webhook update failed",
			"error", err,
			"order_id", event.OrderID,
			"request_id", requestIDFrom(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ownedOrder loads the path order and enforces ownership (404 for non-owners, so the
// response cannot distinguish "not yours" from "does not exist").
func (a *API) ownedOrder(w http.ResponseWriter, r *http.Request) (domain.Order, bool) {
	id := domain.OrderID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "order id is required")
		return domain.Order{}, false
	}
	order, err := a.app.Storage.Orders().Get(r.Context(), id)
	if err != nil {
		a.writeStoreError(w, r, err)
		return domain.Order{}, false
	}
	if !a.ownsOrder(r, order) {
		writeError(w, http.StatusNotFound, "not_found", "requested resource was not found")
		return domain.Order{}, false
	}
	return order, true
}

func (a *API) paymentsProvider() string {
	if provider := a.cfg.Payments.Provider; provider != "" {
		return provider
	}
	return "stub"
}
