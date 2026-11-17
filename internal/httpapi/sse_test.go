package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
)

func newSSEServer(t *testing.T, tune func(*API)) (*httptest.Server, *app.App) {
	t.Helper()
	cfg := testConfig(t)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	api := newAPI(cfg, a, "test-version")
	if tune != nil {
		tune(api)
	}
	ts := httptest.NewServer(api.withMiddleware(api.routes()))
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	return ts, a
}

type sseEvent struct {
	name    string
	data    string
	comment string
}

// startSSEReader parses SSE frames into a channel. Blank-line-delimited frames carry an
// event name and/or data; ": …" lines are heartbeat comments.
func startSSEReader(body interface{ Read([]byte) (int, error) }) <-chan sseEvent {
	out := make(chan sseEvent, 32)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		var current sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				out <- sseEvent{comment: line}
			case line == "":
				if current.name != "" || current.data != "" {
					out <- current
					current = sseEvent{}
				}
			case strings.HasPrefix(line, "event: "):
				current.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

func waitSSE(t *testing.T, ch <-chan sseEvent, match func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("SSE stream ended")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for an SSE frame")
		}
	}
}

func openOrderEvents(t *testing.T, ts *httptest.Server, id domain.OrderID) (*http.Response, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/orders/"+string(id)+"/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open events: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	return resp, startSSEReader(resp.Body)
}

func TestOrderEventsSnapshotThenStream(t *testing.T) {
	ts, a := newSSEServer(t, nil)
	created := createOrder(t, ts, validOrderJSON)

	resp, frames := openOrderEvents(t, ts, created.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("SSE headers = %v", resp.Header)
	}

	snapshot := waitSSE(t, frames, func(ev sseEvent) bool { return ev.name == "order" })
	var order domain.Order
	if err := json.Unmarshal([]byte(snapshot.data), &order); err != nil {
		t.Fatalf("snapshot JSON: %v (%s)", err, snapshot.data)
	}
	if order.ID != created.ID || order.Status != domain.OrderCreated {
		t.Fatalf("snapshot = %+v, want the current order", order)
	}

	// A live event for another order is filtered out; the matching one is streamed.
	if err := a.Events.Publish(context.Background(), events.OrderEvent{
		OrderID: domain.NewOrderID(), Status: domain.OrderPaid, At: time.Now(),
	}); err != nil {
		t.Fatalf("publish other: %v", err)
	}
	if err := a.Events.Publish(context.Background(), events.OrderEvent{
		OrderID: created.ID, Status: domain.OrderScheduled, Attempts: 1, At: time.Now(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	live := waitSSE(t, frames, func(ev sseEvent) bool { return ev.name == "order" })
	var update events.OrderEvent
	if err := json.Unmarshal([]byte(live.data), &update); err != nil {
		t.Fatalf("live JSON: %v (%s)", err, live.data)
	}
	if update.OrderID != created.ID || update.Status != domain.OrderScheduled || update.Attempts != 1 {
		t.Fatalf("live event = %+v", update)
	}
}

func TestOrderEventsHeartbeat(t *testing.T) {
	ts, _ := newSSEServer(t, func(api *API) { api.heartbeat = 10 * time.Millisecond })
	created := createOrder(t, ts, validOrderJSON)
	_, frames := openOrderEvents(t, ts, created.ID)

	waitSSE(t, frames, func(ev sseEvent) bool { return ev.name == "order" })
	ping := waitSSE(t, frames, func(ev sseEvent) bool { return strings.HasPrefix(ev.comment, ": ping") })
	if ping.comment != ": ping" {
		t.Errorf("heartbeat = %q, want ': ping'", ping.comment)
	}
}

type subscriberFunc func(context.Context) (<-chan events.OrderEvent, func(), error)

func (f subscriberFunc) Subscribe(ctx context.Context) (<-chan events.OrderEvent, func(), error) {
	return f(ctx)
}

func TestOrderEventsOverrunEndsStream(t *testing.T) {
	cfg := testConfig(t)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	created, err := a.Storage.Orders().Create(context.Background(), domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis", Phone: "+431"},
		Patient: domain.Patient{FullName: "Max"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed := make(chan events.OrderEvent)
	close(closed)
	api := newAPI(cfg, a, "test")
	api.hub = subscriberFunc(func(context.Context) (<-chan events.OrderEvent, func(), error) {
		return closed, func() {}, nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders/"+string(created.ID)+"/events", nil)
	api.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: order") {
		t.Errorf("missing snapshot event: %s", body)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"code":"overrun"`) {
		t.Errorf("missing overrun frame: %s", body)
	}
}

func TestOrderEventsUnknownOrder(t *testing.T) {
	ts, _ := newSSEServer(t, nil)
	resp := doJSON(t, ts, http.MethodGet, "/api/v1/orders/"+string(domain.NewOrderID())+"/events", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := decodeErr(t, resp).Error.Code; got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}
}

func TestOrderEventsDisconnectStopsHandler(t *testing.T) {
	cfg := testConfig(t)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	created, err := a.Storage.Orders().Create(context.Background(), domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis", Phone: "+431"},
		Patient: domain.Patient{FullName: "Max"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	api := newAPI(cfg, a, "test")
	api.heartbeat = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders/"+string(created.ID)+"/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.routes().ServeHTTP(rec, req)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return after the client disconnected")
	}
}
