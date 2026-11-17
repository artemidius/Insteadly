package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/artemidius/insteadly/internal/core/domain"
)

const (
	testSecretKey     = "sk_test_123"
	testWebhookSecret = "whsec_test_123"
	testPriceID       = "price_test_123"
)

type capturedRequest struct {
	form   url.Values
	header http.Header
}

// newFakeStripe starts an in-repo Stripe API stub. No real Stripe request is ever made.
func newFakeStripe(t *testing.T) (*httptest.Server, func() capturedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		last capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
			http.Error(w, `{"error":{"message":"unexpected path"}}`, http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":{"message":"bad form"}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		last = capturedRequest{form: r.PostForm, header: r.Header.Clone()}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "cs_test_123",
			"object": "checkout.session",
			"url":    "https://checkout.stripe.test/pay/cs_test_123",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func newAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		SecretKey:     testSecretKey,
		WebhookSecret: testWebhookSecret,
		PriceID:       testPriceID,
		SuccessURL:    "https://example.test/dashboard",
		CancelURL:     "https://example.test/orders",
		BaseURL:       baseURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func TestCreateCheckout(t *testing.T) {
	srv, lastRequest := newFakeStripe(t)
	adapter := newAdapter(t, srv.URL)
	order := domain.Order{ID: domain.NewOrderID()}

	redirect, err := adapter.CreateCheckout(context.Background(), order)
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if redirect != "https://checkout.stripe.test/pay/cs_test_123" {
		t.Errorf("redirect = %q", redirect)
	}

	got := lastRequest()
	if auth := got.header.Get("Authorization"); !strings.Contains(auth, testSecretKey) {
		t.Errorf("Authorization = %q, want the secret key", auth)
	}
	wants := map[string]string{
		"mode":                    "payment",
		"success_url":             "https://example.test/dashboard?order=" + string(order.ID),
		"cancel_url":              "https://example.test/orders",
		"client_reference_id":     string(order.ID),
		"line_items[0][price]":    testPriceID,
		"line_items[0][quantity]": "1",
		"metadata[order_id]":      string(order.ID),
	}
	for field, want := range wants {
		if got := got.form.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

func TestCreateCheckoutRequiresOrderID(t *testing.T) {
	adapter := newAdapter(t, "")
	if _, err := adapter.CreateCheckout(context.Background(), domain.Order{}); err == nil {
		t.Fatal("CreateCheckout with an empty order id succeeded")
	}
}

func signedPayload(t *testing.T, eventType string, orderID string) *webhook.SignedPayload {
	t.Helper()
	payload := fmt.Sprintf(`{
		"id": "evt_test_123",
		"object": "event",
		"api_version": "2020-01-01.basil",
		"type": %q,
		"data": {"object": {
			"id": "cs_test_123",
			"object": "checkout.session",
			"client_reference_id": %q,
			"metadata": {"order_id": %q}
		}}
	}`, eventType, orderID, orderID)
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(payload),
		Secret:  testWebhookSecret,
	})
}

func TestHandleWebhookCheckoutCompleted(t *testing.T) {
	adapter := newAdapter(t, "")
	orderID := domain.NewOrderID()
	signed := signedPayload(t, "checkout.session.completed", string(orderID))

	event, err := adapter.HandleWebhook(context.Background(), signed.Payload, signed.Header)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if !event.Paid {
		t.Error("Paid = false")
	}
	if event.OrderID != orderID {
		t.Errorf("OrderID = %q, want %q", event.OrderID, orderID)
	}
	if event.Reference != "cs_test_123" {
		t.Errorf("Reference = %q, want the checkout session id", event.Reference)
	}
	if event.At.IsZero() {
		t.Error("At is zero")
	}
}

func TestHandleWebhookFallsBackToClientReferenceID(t *testing.T) {
	adapter := newAdapter(t, "")
	orderID := domain.NewOrderID()
	payload := fmt.Sprintf(`{
		"id":"evt_test_123","object":"event","api_version":"2020-01-01.basil",
		"type":"checkout.session.completed",
		"data":{"object":{"id":"cs_test_123","object":"checkout.session","client_reference_id":%q}}
	}`, string(orderID))
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(payload), Secret: testWebhookSecret,
	})
	event, err := adapter.HandleWebhook(context.Background(), signed.Payload, signed.Header)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if event.OrderID != orderID || !event.Paid {
		t.Errorf("event = %+v", event)
	}
}

func TestHandleWebhookBadSignature(t *testing.T) {
	adapter := newAdapter(t, "")
	orderID := domain.NewOrderID()
	signed := signedPayload(t, "checkout.session.completed", string(orderID))

	t.Run("tampered payload", func(t *testing.T) {
		tampered := append([]byte{}, signed.Payload...)
		tampered[len(tampered)-2] ^= 0x01
		if _, err := adapter.HandleWebhook(context.Background(), tampered, signed.Header); err == nil {
			t.Fatal("tampered payload accepted")
		}
	})
	t.Run("missing signature", func(t *testing.T) {
		if _, err := adapter.HandleWebhook(context.Background(), signed.Payload, ""); err == nil {
			t.Fatal("missing signature accepted")
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		wrong := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: signed.Payload, Secret: "whsec_wrong",
		})
		if _, err := adapter.HandleWebhook(context.Background(), signed.Payload, wrong.Header); err == nil {
			t.Fatal("signature from the wrong secret accepted")
		}
	})
}

func TestHandleWebhookIgnoresOtherEvents(t *testing.T) {
	adapter := newAdapter(t, "")
	signed := signedPayload(t, "customer.created", string(domain.NewOrderID()))
	event, err := adapter.HandleWebhook(context.Background(), signed.Payload, signed.Header)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if event.Paid || event.Refunded || event.OrderID != "" || event.Reference != "" {
		t.Errorf("ignored event produced %+v", event)
	}
}

func TestHandleWebhookUncorrelatedSessionIsAcked(t *testing.T) {
	adapter := newAdapter(t, "")
	payload := `{"id":"evt_test_123","object":"event","api_version":"2020-01-01.basil",
		"type":"checkout.session.completed","data":{"object":{"id":"cs_test_123","object":"checkout.session"}}}`
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(payload), Secret: testWebhookSecret,
	})
	event, err := adapter.HandleWebhook(context.Background(), signed.Payload, signed.Header)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if event.Paid || event.OrderID != "" {
		t.Errorf("uncorrelated event produced %+v", event)
	}
}

func TestNewValidation(t *testing.T) {
	valid := Config{
		SecretKey:     testSecretKey,
		WebhookSecret: testWebhookSecret,
		PriceID:       testPriceID,
		SuccessURL:    "https://example.test/dashboard",
		CancelURL:     "https://example.test/orders",
	}
	cases := map[string]func(*Config){
		"missing secret key":     func(c *Config) { c.SecretKey = " " },
		"missing webhook secret": func(c *Config) { c.WebhookSecret = "" },
		"missing price id":       func(c *Config) { c.PriceID = "" },
		"missing success url":    func(c *Config) { c.SuccessURL = "" },
		"missing cancel url":     func(c *Config) { c.CancelURL = "" },
		"bad success url":        func(c *Config) { c.SuccessURL = "not-a-url" },
		"bad cancel url":         func(c *Config) { c.CancelURL = "ftp://example.test/x" },
		"bad base url":           func(c *Config) { c.BaseURL = "nope" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatal("New succeeded, want rejection")
			}
			for _, secret := range []string{testSecretKey, testWebhookSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %v", err)
				}
			}
		})
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSuccessURLPreservesQuery(t *testing.T) {
	id := domain.OrderID("abc")
	if got := successURL("https://example.test/dashboard?lang=de", id); got != "https://example.test/dashboard?lang=de&order=abc" {
		t.Errorf("successURL = %q", got)
	}
	if got := successURL("https://example.test/dashboard/", id); got != "https://example.test/dashboard?order=abc" {
		t.Errorf("successURL = %q", got)
	}
}

func TestNewDefaultLoggerDoesNotPanic(t *testing.T) {
	adapter, err := New(Config{
		SecretKey: testSecretKey, WebhookSecret: testWebhookSecret, PriceID: testPriceID,
		SuccessURL: "https://example.test/d", CancelURL: "https://example.test/c",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	event, err := adapter.HandleWebhook(context.Background(), []byte("{}"), "")
	if err == nil || event.Paid {
		t.Errorf("empty payload should fail signature verification, got %+v err=%v", event, err)
	}
}
