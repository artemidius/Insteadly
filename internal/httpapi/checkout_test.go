package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
)

const httpWebhookSecret = "whsec_http_test_123"

func stripeServerConfig(c *config.Config) {
	cookieAuthConfig(c)
	c.HTTP.PublicURL = "https://insteadly.example.test"
	c.Payments = config.Payments{
		Provider: "stripe",
		Stripe: config.Stripe{
			SecretKey:     "sk_test_http",
			WebhookSecret: httpWebhookSecret,
			PriceID:       "price_http_test",
		},
	}
}

func createOrderWithCookie(t *testing.T, ts *httptest.Server, cookie *http.Cookie) domain.Order {
	t.Helper()
	resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/orders", validOrderJSON, cookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order status = %d, want 201", resp.StatusCode)
	}
	return decodeOrder(t, resp)
}

func getOrderWithCookie(t *testing.T, ts *httptest.Server, id domain.OrderID, cookie *http.Cookie) (*http.Response, domain.Order) {
	t.Helper()
	resp := doWithCookie(t, ts, http.MethodGet, "/api/v1/orders/"+string(id), "", cookie)
	var order domain.Order
	if resp.StatusCode == http.StatusOK {
		order = decodeOrder(t, resp)
	}
	return resp, order
}

func TestOrderOwnershipScoping(t *testing.T) {
	ts, _ := newCookieServer(t)
	cookieA, userA := registerUser(t, ts, "owner@example.test", "password123")
	cookieB, _ := registerUser(t, ts, "other@example.test", "password123")

	order := createOrderWithCookie(t, ts, cookieA)
	if order.UserID != userA.ID {
		t.Fatalf("order user_id = %q, want %q", order.UserID, userA.ID)
	}

	// A sees the order in its list; B's list is empty.
	resp := doWithCookie(t, ts, http.MethodGet, "/api/v1/orders", "", cookieA)
	var listA []domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&listA); err != nil {
		t.Fatalf("decode A list: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != order.ID {
		t.Fatalf("A list = %+v", listA)
	}
	resp = doWithCookie(t, ts, http.MethodGet, "/api/v1/orders", "", cookieB)
	var listB []domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&listB); err != nil {
		t.Fatalf("decode B list: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("B list = %+v, want empty", listB)
	}

	// Non-owner get/events/checkout are indistinguishable from a missing order.
	resp, _ = getOrderWithCookie(t, ts, order.ID, cookieB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B get status = %d, want 404", resp.StatusCode)
	}
	resp = doWithCookie(t, ts, http.MethodGet, "/api/v1/orders/"+string(order.ID)+"/events", "", cookieB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B events status = %d, want 404", resp.StatusCode)
	}
	resp = doWithCookie(t, ts, http.MethodPost, "/api/v1/orders/"+string(order.ID)+"/checkout", "", cookieB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B checkout status = %d, want 404", resp.StatusCode)
	}

	// Owner can fetch and start a checkout.
	resp, got := getOrderWithCookie(t, ts, order.ID, cookieA)
	if resp.StatusCode != http.StatusOK || got.ID != order.ID {
		t.Fatalf("A get status = %d order = %+v", resp.StatusCode, got)
	}
	resp = doWithCookie(t, ts, http.MethodPost, "/api/v1/orders/"+string(order.ID)+"/checkout", "", cookieA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A checkout status = %d, want 200", resp.StatusCode)
	}
	var redirect struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&redirect); err != nil {
		t.Fatalf("decode checkout: %v", err)
	}
	wantURL := "/api/v1/orders/" + string(order.ID) + "/checkout/simulate"
	if redirect.RedirectURL != wantURL {
		t.Errorf("redirect_url = %q, want %q", redirect.RedirectURL, wantURL)
	}

	// Non-owner cannot simulate the payment.
	resp = doWithCookie(t, ts, http.MethodGet, "/api/v1/orders/"+string(order.ID)+"/checkout/simulate", "", cookieB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B simulate status = %d, want 404", resp.StatusCode)
	}
}

func TestStubSimulateMarksPaid(t *testing.T) {
	ts, _ := newCookieServer(t)
	cookie, _ := registerUser(t, ts, "pay@example.test", "password123")
	order := createOrderWithCookie(t, ts, cookie)

	client := *ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/orders/"+string(order.ID)+"/checkout/simulate", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/dashboard?order="+string(order.ID) {
		t.Errorf("Location = %q", loc)
	}

	_, paid := getOrderWithCookie(t, ts, order.ID, cookie)
	if paid.Status != domain.OrderPaid {
		t.Fatalf("status = %q, want paid", paid.Status)
	}
	if paid.PaidAt == nil {
		t.Error("paid_at is nil")
	}
	if paid.PaymentReference == nil || *paid.PaymentReference != "stub/"+string(order.ID) {
		t.Errorf("payment_reference = %v", paid.PaymentReference)
	}

	// A second simulate is a no-op (MarkPaid is idempotent past created).
	resp2, err := client.Do(req.Clone(context.Background()))
	if err != nil {
		t.Fatalf("second simulate: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("second simulate status = %d, want 303", resp2.StatusCode)
	}
	_, again := getOrderWithCookie(t, ts, order.ID, cookie)
	if again.PaidAt == nil || !again.PaidAt.Equal(*paid.PaidAt) ||
		again.PaymentReference == nil || *again.PaymentReference != *paid.PaymentReference {
		t.Errorf("simulate replay changed payment: before %+v after %+v", paid, again)
	}
}

func TestCreateOrderRejectsServerAssignedPaymentFields(t *testing.T) {
	ts, _ := newCookieServer(t)
	cookie, _ := registerUser(t, ts, "fields@example.test", "password123")
	cases := map[string]string{
		"user_id":           `{"user_id":"0123456789abcdef0123456789abcdef","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`,
		"paid_at":           `{"paid_at":"2026-11-17T09:00:00Z","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`,
		"payment_reference": `{"payment_reference":"cs_x","clinic":{"name":"P","phone":"+431"},"patient":{"full_name":"X"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := doWithCookie(t, ts, http.MethodPost, "/api/v1/orders", body, cookie)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if code := decodeErr(t, resp).Error.Code; code != "bad_request" {
				t.Errorf("code = %q", code)
			}
		})
	}
}

func stripeSignedPayload(t *testing.T, eventType string, orderID domain.OrderID) *webhook.SignedPayload {
	t.Helper()
	payload := fmt.Sprintf(`{
		"id":"evt_http_123","object":"event","api_version":"2020-01-01.basil",
		"type":%q,
		"data":{"object":{"id":"cs_http_123","object":"checkout.session",
			"client_reference_id":%q,"metadata":{"order_id":%q}}}
	}`, eventType, string(orderID), string(orderID))
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(payload),
		Secret:  httpWebhookSecret,
	})
}

func postStripeWebhook(t *testing.T, ts *httptest.Server, payload []byte, signature string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/payments/stripe", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signature)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestStripeWebhookMarksPaidAndReplayIsNoOp(t *testing.T) {
	ts, _ := newTestServerWithApp(t, stripeServerConfig)
	cookie, _ := registerUser(t, ts, "stripe@example.test", "password123")
	order := createOrderWithCookie(t, ts, cookie)

	signed := stripeSignedPayload(t, "checkout.session.completed", order.ID)
	resp := postStripeWebhook(t, ts, signed.Payload, signed.Header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", resp.StatusCode)
	}

	_, paid := getOrderWithCookie(t, ts, order.ID, cookie)
	if paid.Status != domain.OrderPaid {
		t.Fatalf("status = %q, want paid", paid.Status)
	}
	if paid.PaidAt == nil || paid.PaymentReference == nil || *paid.PaymentReference != "cs_http_123" {
		t.Fatalf("payment fields = paid_at %v ref %v", paid.PaidAt, paid.PaymentReference)
	}

	// Stripe redelivers webhooks; the second delivery must not change anything.
	resp = postStripeWebhook(t, ts, signed.Payload, signed.Header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", resp.StatusCode)
	}
	_, again := getOrderWithCookie(t, ts, order.ID, cookie)
	if again.PaidAt == nil || !again.PaidAt.Equal(*paid.PaidAt) ||
		again.PaymentReference == nil || *again.PaymentReference != *paid.PaymentReference {
		t.Errorf("webhook replay changed payment: before %+v after %+v", paid, again)
	}
}

func TestStripeWebhookBadSignature(t *testing.T) {
	ts, _ := newTestServerWithApp(t, stripeServerConfig)
	cookie, _ := registerUser(t, ts, "badsig@example.test", "password123")
	order := createOrderWithCookie(t, ts, cookie)

	signed := stripeSignedPayload(t, "checkout.session.completed", order.ID)
	tampered := append([]byte{}, signed.Payload...)
	tampered[len(tampered)-2] ^= 0x01
	resp := postStripeWebhook(t, ts, tampered, signed.Header)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad signature status = %d, want 400", resp.StatusCode)
	}
	resp = postStripeWebhook(t, ts, signed.Payload, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing signature status = %d, want 400", resp.StatusCode)
	}
	_, unchanged := getOrderWithCookie(t, ts, order.ID, cookie)
	if unchanged.Status != domain.OrderCreated || unchanged.PaidAt != nil {
		t.Errorf("bad signature mutated the order: %+v", unchanged)
	}
}

func TestStripeWebhookUnknownOrderAndOtherEventsAck(t *testing.T) {
	ts, _ := newTestServerWithApp(t, stripeServerConfig)

	unknown := stripeSignedPayload(t, "checkout.session.completed", domain.NewOrderID())
	if resp := postStripeWebhook(t, ts, unknown.Payload, unknown.Header); resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown order status = %d, want 200", resp.StatusCode)
	}

	other := stripeSignedPayload(t, "customer.created", domain.NewOrderID())
	if resp := postStripeWebhook(t, ts, other.Payload, other.Header); resp.StatusCode != http.StatusOK {
		t.Fatalf("other event status = %d, want 200", resp.StatusCode)
	}
}

func TestStripeWebhookNotMountedForStub(t *testing.T) {
	ts, _ := newCookieServer(t)
	resp := postStripeWebhook(t, ts, []byte(`{}`), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stub-mode webhook status = %d, want 404", resp.StatusCode)
	}
}
