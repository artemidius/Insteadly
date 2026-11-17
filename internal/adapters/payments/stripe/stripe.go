// Package stripe implements ports.Payments with Stripe Checkout Sessions and signed
// webhooks. Stripe types never leave this package: the core sees only redirect URLs and
// domain.PaymentEvent.
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Config configures the Stripe adapter. SecretKey and WebhookSecret are secrets and are
// never logged.
type Config struct {
	SecretKey     string // sk_… (secret; never logged)
	WebhookSecret string // whsec_… (secret; never logged)
	PriceID       string // one Stripe Price id for the prototype
	SuccessURL    string // base URL Stripe returns the payer to; the adapter appends ?order=<id>
	CancelURL     string // absolute URL on cancel
	BaseURL       string // test-only override of the API origin (fake server); "" = default
	Logger        *slog.Logger
}

// Adapter is the Stripe implementation of ports.Payments.
type Adapter struct {
	cfg    Config
	client *stripe.Client
	log    *slog.Logger
}

var _ ports.Payments = (*Adapter)(nil)

// New validates the configuration and builds a Stripe client. It performs no network I/O.
func New(cfg Config) (*Adapter, error) {
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	cfg.PriceID = strings.TrimSpace(cfg.PriceID)
	cfg.SuccessURL = strings.TrimSpace(cfg.SuccessURL)
	cfg.CancelURL = strings.TrimSpace(cfg.CancelURL)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	switch {
	case cfg.SecretKey == "":
		return nil, fmt.Errorf("stripe: secret key is required")
	case cfg.WebhookSecret == "":
		return nil, fmt.Errorf("stripe: webhook secret is required")
	case cfg.PriceID == "":
		return nil, fmt.Errorf("stripe: price id is required")
	case cfg.SuccessURL == "":
		return nil, fmt.Errorf("stripe: success url is required")
	case cfg.CancelURL == "":
		return nil, fmt.Errorf("stripe: cancel url is required")
	}
	for name, raw := range map[string]string{"success url": cfg.SuccessURL, "cancel url": cfg.CancelURL} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("stripe: %s must be an absolute http(s) URL", name)
		}
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}

	client := stripe.NewClient(cfg.SecretKey)
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("stripe: base url must be an absolute http(s) URL")
		}
		// Tests point the SDK at an in-repo httptest server. Disable retries and telemetry
		// so a test never reaches the real Stripe API or sends metrics.
		backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
			URL:               stripe.String(cfg.BaseURL),
			MaxNetworkRetries: stripe.Int64(0),
			EnableTelemetry:   stripe.Bool(false),
		})
		client = stripe.NewClient(cfg.SecretKey, stripe.WithBackends(backends))
	}
	return &Adapter{cfg: cfg, client: client, log: log}, nil
}

// CreateCheckout creates a one-time Checkout Session and returns its hosted URL.
func (a *Adapter) CreateCheckout(ctx context.Context, order domain.Order) (string, error) {
	if strings.TrimSpace(string(order.ID)) == "" {
		return "", fmt.Errorf("stripe: order id is required")
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL:        stripe.String(successURL(a.cfg.SuccessURL, order.ID)),
		CancelURL:         stripe.String(a.cfg.CancelURL),
		ClientReferenceID: stripe.String(string(order.ID)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			Price:    stripe.String(a.cfg.PriceID),
			Quantity: stripe.Int64(1),
		}},
		Metadata: map[string]string{"order_id": string(order.ID)},
	}
	session, err := a.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe: create checkout session: %w", err)
	}
	if strings.TrimSpace(session.URL) == "" {
		return "", fmt.Errorf("stripe: checkout session %s has no URL", session.ID)
	}
	return session.URL, nil
}

// HandleWebhook verifies the Stripe signature and normalizes checkout.session.completed.
// Any other event type is ignored (zero PaymentEvent). An invalid signature is an error;
// the HTTP layer maps it to 400.
func (a *Adapter) HandleWebhook(_ context.Context, payload []byte, signature string) (domain.PaymentEvent, error) {
	event, err := webhook.ConstructEventWithOptions(payload, signature, a.cfg.WebhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return domain.PaymentEvent{}, fmt.Errorf("stripe: verify webhook: %w", err)
	}
	if event.Type != stripe.EventTypeCheckoutSessionCompleted {
		a.log.Debug("stripe: ignoring webhook event", "type", string(event.Type))
		return domain.PaymentEvent{}, nil
	}
	if event.Data == nil {
		return domain.PaymentEvent{}, fmt.Errorf("stripe: checkout.session.completed has no data")
	}
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return domain.PaymentEvent{}, fmt.Errorf("stripe: decode checkout session: %w", err)
	}
	orderID := strings.TrimSpace(session.Metadata["order_id"])
	if orderID == "" {
		orderID = strings.TrimSpace(session.ClientReferenceID)
	}
	if orderID == "" {
		// A session we cannot correlate is acknowledged and ignored; retrying would not
		// make an order_id appear.
		a.log.Warn("stripe: checkout session completed without an order reference", "session_id", session.ID)
		return domain.PaymentEvent{}, nil
	}
	return domain.PaymentEvent{
		OrderID:   domain.OrderID(orderID),
		Paid:      true,
		At:        time.Now().UTC(),
		Reference: session.ID,
	}, nil
}

// successURL appends ?order=<id> to the configured dashboard URL so Stripe returns the
// payer to the right order. Existing query strings are preserved.
func successURL(base string, id domain.OrderID) string {
	base = strings.TrimRight(base, "/")
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + "order=" + url.QueryEscape(string(id))
}

// nopWriter discards the SDK's default logger output while keeping the adapter quiet.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
