// Package stub implements ports.Payments for development: "checkout" redirects to an
// authenticated simulate endpoint that drives the same domain.MarkPaid seam a real
// webhook uses. It makes the whole scheduler flow testable with no Stripe account.
package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Adapter is the stub payments provider. It holds no resources.
type Adapter struct {
	log *slog.Logger
}

var _ ports.Payments = (*Adapter)(nil)

// New builds the stub adapter. A nil logger discards.
func New(log *slog.Logger) *Adapter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Adapter{log: log}
}

// CreateCheckout returns the in-app simulate URL: the browser navigates there, and the
// authenticated GET handler marks the order paid.
func (a *Adapter) CreateCheckout(_ context.Context, order domain.Order) (string, error) {
	if strings.TrimSpace(string(order.ID)) == "" {
		return "", fmt.Errorf("stub payments: order id is required")
	}
	return "/api/v1/orders/" + string(order.ID) + "/checkout/simulate", nil
}

// stubWebhook is the trivial payload the stub adapter accepts in tests.
type stubWebhook struct {
	OrderID   string `json:"order_id"`
	Paid      bool   `json:"paid"`
	Reference string `json:"reference,omitempty"`
}

// HandleWebhook parses a deliberately simple stub payload. There is no signature to
// verify: this adapter is only selected explicitly for development and no webhook route
// is mounted for it.
func (a *Adapter) HandleWebhook(_ context.Context, payload []byte, _ string) (domain.PaymentEvent, error) {
	var body stubWebhook
	if err := json.Unmarshal(payload, &body); err != nil {
		return domain.PaymentEvent{}, fmt.Errorf("stub payments: decode webhook: %w", err)
	}
	orderID := strings.TrimSpace(body.OrderID)
	if orderID == "" {
		return domain.PaymentEvent{}, fmt.Errorf("stub payments: order_id is required")
	}
	reference := strings.TrimSpace(body.Reference)
	if reference == "" {
		reference = "stub/" + orderID
	}
	event := domain.PaymentEvent{
		OrderID:   domain.OrderID(orderID),
		Paid:      body.Paid,
		At:        time.Now().UTC(),
		Reference: reference,
	}
	a.log.Debug("stub payments: webhook", "order_id", orderID, "paid", body.Paid)
	return event, nil
}
