package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
)

func newTestServerWithApp(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *app.App) {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ts := httptest.NewServer(New(cfg, a, "test-version").Handler)
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	return ts, a
}

func doJSON(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func placeCall(t *testing.T, ts *httptest.Server, id domain.OrderID, headers map[string]string) *http.Response {
	t.Helper()
	return doJSON(t, ts, http.MethodPost, "/api/v1/orders/"+string(id)+"/call", "", headers)
}

func TestPlaceCallRequiresAPIKeyWhenConfigured(t *testing.T) {
	const key = "place-call-secret"
	ts, _ := newTestServerWithApp(t, func(c *config.Config) { c.API.Key = key })
	created := createOrder(t, ts, validOrderJSON)

	resp := placeCall(t, ts, created.ID, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", code)
	}

	resp = placeCall(t, ts, created.ID, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-key status = %d, want 401", resp.StatusCode)
	}

	resp = placeCall(t, ts, created.ID, map[string]string{"Authorization": "Bearer " + key})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid-key status = %d, want 202", resp.StatusCode)
	}
	scheduled := decodeOrder(t, resp)
	if scheduled.Status != domain.OrderScheduled {
		t.Errorf("status = %q, want scheduled", scheduled.Status)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/orders/"+string(created.ID) {
		t.Errorf("Location = %q", loc)
	}
}

func TestPlaceCallAuthDisabledWithoutKey(t *testing.T) {
	ts, _ := newTestServerWithApp(t, nil)
	created := createOrder(t, ts, validOrderJSON)
	resp := placeCall(t, ts, created.ID, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (auth disabled when key is empty)", resp.StatusCode)
	}
}

func TestPlaceCallSchedulesOnceAndRejectsRepeats(t *testing.T) {
	ts, _ := newTestServerWithApp(t, nil)
	created := createOrder(t, ts, validOrderJSON)

	resp := placeCall(t, ts, created.ID, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	scheduled := decodeOrder(t, resp)
	if scheduled.Status != domain.OrderScheduled || scheduled.NextAttemptAt == nil {
		t.Fatalf("scheduled = %+v", scheduled)
	}

	// A second request without an idempotency key is a conflict, never a second dial.
	resp = placeCall(t, ts, created.ID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second call status = %d, want 409", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "already_scheduled" {
		t.Errorf("code = %q, want already_scheduled", code)
	}
}

func TestPlaceCallIdempotentReplay(t *testing.T) {
	ts, _ := newTestServerWithApp(t, nil)
	created := createOrder(t, ts, validOrderJSON)
	key := map[string]string{"Idempotency-Key": "patient-request-1"}

	first := placeCall(t, ts, created.ID, key)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}
	firstOrder := decodeOrder(t, first)

	second := placeCall(t, ts, created.ID, key)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", second.StatusCode)
	}
	secondOrder := decodeOrder(t, second)
	if secondOrder.ID != firstOrder.ID || secondOrder.Status != firstOrder.Status ||
		secondOrder.NextAttemptAt == nil || !secondOrder.NextAttemptAt.Equal(*firstOrder.NextAttemptAt) {
		t.Errorf("replay returned a different result: %+v vs %+v", secondOrder, firstOrder)
	}

	// A different key is a normal request and conflicts with the scheduled order.
	other := placeCall(t, ts, created.ID, map[string]string{"Idempotency-Key": "patient-request-2"})
	if other.StatusCode != http.StatusConflict {
		t.Fatalf("different-key status = %d, want 409", other.StatusCode)
	}
}

func TestPlaceCallIdempotencyExpiry(t *testing.T) {
	ts, _ := newTestServerWithApp(t, func(c *config.Config) {
		c.API.IdempotencyTTL = 20 * time.Millisecond
	})
	created := createOrder(t, ts, validOrderJSON)
	key := map[string]string{"Idempotency-Key": "expiring"}

	if resp := placeCall(t, ts, created.ID, key); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", resp.StatusCode)
	}
	time.Sleep(40 * time.Millisecond)
	// After expiry the key is no longer remembered; the normal path sees a scheduled
	// order and answers 409 rather than replaying.
	resp := placeCall(t, ts, created.ID, key)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post-expiry status = %d, want 409", resp.StatusCode)
	}
}

func TestPlaceCallErrors(t *testing.T) {
	ts, a := newTestServerWithApp(t, nil)
	created := createOrder(t, ts, validOrderJSON)

	resp := placeCall(t, ts, domain.NewOrderID(), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing order status = %d, want 404", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "not_found" {
		t.Errorf("code = %q", code)
	}

	long := strings.Repeat("k", maxIdempotencyKey+1)
	resp = placeCall(t, ts, created.ID, map[string]string{"Idempotency-Key": long})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long key status = %d, want 400", resp.StatusCode)
	}

	// A finished order is a hard conflict.
	finished := createOrder(t, ts, validOrderJSON)
	if _, err := a.Storage.Orders().Update(context.Background(), finished.ID, func(o *domain.Order) error {
		return o.Finish(domain.Outcome{Kind: domain.OutcomeAborted})
	}); err != nil {
		t.Fatalf("finish fixture: %v", err)
	}
	resp = placeCall(t, ts, finished.ID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("finished order status = %d, want 409", resp.StatusCode)
	}
	if code := decodeErr(t, resp).Error.Code; code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}
}

func TestPlaceCallBareBearerRejected(t *testing.T) {
	ts, _ := newTestServerWithApp(t, func(c *config.Config) { c.API.Key = "secret" })
	created := createOrder(t, ts, validOrderJSON)
	for _, header := range []string{"Bearer", "Bearer ", "Basic secret", "secret"} {
		resp := placeCall(t, ts, created.ID, map[string]string{"Authorization": header})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", header, resp.StatusCode)
		}
	}
}

func TestPlaceCallResponseIsAnOrder(t *testing.T) {
	ts, _ := newTestServerWithApp(t, nil)
	created := createOrder(t, ts, validOrderJSON)
	resp := placeCall(t, ts, created.ID, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var order domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if order.ID != created.ID || order.Status != domain.OrderScheduled {
		t.Fatalf("order = %+v", order)
	}
	if order.Outcome != nil || len(order.AttemptHistory) != 0 {
		t.Errorf("scheduling should not set an outcome/history: %+v", order)
	}
}
