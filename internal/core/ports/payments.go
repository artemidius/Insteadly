package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Payments charges and refunds orders (DESIGN.md §2.2). First adapter: Stripe, C6.
type Payments interface {
	// CreateCheckout starts a payment for the order and returns a redirect URL.
	CreateCheckout(ctx context.Context, order domain.Order) (redirectURL string, err error)

	// HandleWebhook processes a provider webhook. payload is the raw body,
	// signature the provider's signature header. Never trust payload alone.
	HandleWebhook(ctx context.Context, payload []byte, signature string) (domain.PaymentEvent, error)
}
