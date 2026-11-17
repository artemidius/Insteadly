package stub

import (
	"context"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestCreateCheckoutReturnsSimulateURL(t *testing.T) {
	adapter := New(nil)
	order := domain.Order{ID: domain.NewOrderID()}
	url, err := adapter.CreateCheckout(context.Background(), order)
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	want := "/api/v1/orders/" + string(order.ID) + "/checkout/simulate"
	if url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
	if _, err := adapter.CreateCheckout(context.Background(), domain.Order{}); err == nil {
		t.Error("empty order id accepted")
	}
}

func TestHandleWebhook(t *testing.T) {
	adapter := New(nil)
	orderID := domain.NewOrderID()
	event, err := adapter.HandleWebhook(context.Background(),
		[]byte(`{"order_id":"`+string(orderID)+`","paid":true}`), "")
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if event.OrderID != orderID || !event.Paid {
		t.Errorf("event = %+v", event)
	}
	if event.Reference != "stub/"+string(orderID) {
		t.Errorf("reference = %q", event.Reference)
	}
	if event.At.IsZero() {
		t.Error("At is zero")
	}
}

func TestHandleWebhookExplicitReferenceAndUnpaid(t *testing.T) {
	adapter := New(nil)
	orderID := domain.NewOrderID()
	event, err := adapter.HandleWebhook(context.Background(),
		[]byte(`{"order_id":"`+string(orderID)+`","paid":false,"reference":"custom-ref"}`), "")
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if event.Paid || event.Reference != "custom-ref" {
		t.Errorf("event = %+v", event)
	}
}

func TestHandleWebhookErrors(t *testing.T) {
	adapter := New(nil)
	for name, payload := range map[string]string{
		"bad json":       `{`,
		"missing order":  `{"paid":true}`,
		"blank order id": `{"order_id":"  ","paid":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.HandleWebhook(context.Background(), []byte(payload), ""); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
