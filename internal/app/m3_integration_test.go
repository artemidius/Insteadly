package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/adapters/payments/stub"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/app"
	coreauth "github.com/artemidius/insteadly/internal/core/auth"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
	"github.com/artemidius/insteadly/internal/core/scheduler"
	"github.com/artemidius/insteadly/internal/httpapi"
)

// m3Hasher is a deliberately trivial PasswordHasher for the integration harness; bcrypt
// has its own adapter tests and would only slow this end-to-end flow down.
type m3Hasher struct{}

func (m3Hasher) Hash(_ context.Context, password string) (string, error) {
	return "hash:" + password, nil
}

func (m3Hasher) Verify(_ context.Context, password, hash string) bool {
	return hash == "hash:"+password
}

type m3Harness struct {
	t         *testing.T
	store     *local.Store
	clock     *scheduler.FakeClock
	hub       *events.Hub
	app       *app.App
	server    *httptest.Server
	telephony *m2Telephony
	agent     *m2Agent
}

func m3TestConfig(authMode string) config.Config {
	cfg := m2TestConfig()
	cfg.Auth = config.Auth{
		Mode:       authMode,
		SessionTTL: time.Hour,
		CookieName: "insteadly_session",
		BcryptCost: 4,
	}
	cfg.Payments = config.Payments{Provider: "stub"}
	return cfg
}

func newM3Harness(t *testing.T, authMode string, scripts []m2Script, outcomes []domain.Outcome) *m3Harness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	clock := scheduler.NewFakeClock(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)) // Monday
	hub := events.NewHub(64)
	telephony := &m2Telephony{scripts: scripts}
	fakeAgent := &m2Agent{outcomes: outcomes}

	run, err := runner.New(runner.Config{
		Telephony:      telephony,
		Orders:         store.Orders(),
		Agent:          fakeAgent,
		Logger:         logger,
		Events:         hub,
		CallTimeout:    5 * time.Second,
		CleanupTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	sched, err := scheduler.New(scheduler.Config{
		Store:           store,
		Orders:          store.Orders(),
		Runner:          run,
		Events:          hub,
		Clock:           clock,
		Windows:         mustWindows(t),
		Tick:            2 * time.Millisecond,
		MaxAttempts:     3,
		MaxConcurrent:   4,
		Lease:           time.Minute,
		BusyBackoff:     15 * time.Minute,
		NoAnswerBackoff: 30 * time.Minute,
		ErrorBackoff:    5 * time.Minute,
		WorkerID:        "m3-test",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	cfg := m3TestConfig(authMode)
	application := &app.App{
		Config:    cfg,
		Storage:   store,
		Telephony: telephony,
		Log:       logger,
		Runner:    run,
		Scheduler: sched,
		Events:    hub,
		Auth:      coreauth.New(store, m3Hasher{}, time.Hour),
		Payments:  stub.New(logger),
	}
	application.Start(context.Background())
	server := httptest.NewServer(httpapi.New(cfg, application, "m3-test").Handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := application.Close(ctx); err != nil {
			t.Errorf("app.Close: %v", err)
		}
		server.Close()
		if err := application.CloseStorage(); err != nil {
			t.Errorf("CloseStorage: %v", err)
		}
	})
	return &m3Harness{
		t: t, store: store, clock: clock, hub: hub, app: application,
		server: server, telephony: telephony, agent: fakeAgent,
	}
}

func (h *m3Harness) do(method, path, body string, cookie *http.Cookie) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *m3Harness) register(email, password string) *http.Cookie {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("register status = %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "insteadly_session" {
			return cookie
		}
	}
	h.t.Fatal("register did not set a session cookie")
	return nil
}

func (h *m3Harness) createOrder(cookie *http.Cookie) domain.Order {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/orders", m2OrderJSON, cookie)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create status = %d", resp.StatusCode)
	}
	var order domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		h.t.Fatalf("decode order: %v", err)
	}
	return order
}

func (h *m3Harness) waitOrder(id domain.OrderID, timeout time.Duration, pred func(domain.Order) bool) domain.Order {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		order, err := h.store.Orders().Get(context.Background(), id)
		if err == nil && pred(order) {
			return order
		}
		time.Sleep(2 * time.Millisecond)
	}
	order, err := h.store.Orders().Get(context.Background(), id)
	h.t.Fatalf("timed out waiting for order state; last = %+v err = %v", order, err)
	return domain.Order{}
}

func (h *m3Harness) openEvents(id domain.OrderID, cookie *http.Cookie) <-chan m2Event {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+"/api/v1/orders/"+string(id)+"/events", nil)
	if err != nil {
		cancel()
		h.t.Fatalf("new SSE request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("open SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		h.t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	out := make(chan m2Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var current m2Event
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				current.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			case line == "" && current.name == "order":
				var status struct {
					Status domain.OrderStatus `json:"status"`
				}
				_ = json.Unmarshal([]byte(current.data), &status)
				current.status = status.Status
				out <- current
				current = m2Event{}
			case line == "":
				current = m2Event{}
			}
		}
	}()
	h.t.Cleanup(cancel)
	return out
}

func TestM3IntegrationRegisterCheckoutThenBooked(t *testing.T) {
	bookedAt := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	outcomes := []domain.Outcome{{
		Kind:       domain.OutcomeBooked,
		BookedSlot: &domain.BookedSlot{DateTime: bookedAt, Doctor: "Dr. Huber", Practice: "Praxis M3"},
	}}
	h := newM3Harness(t, "cookie", []m2Script{{answer: true}}, outcomes)

	cookie := h.register("m3@example.test", "password123")
	order := h.createOrder(cookie)
	if order.UserID == "" {
		t.Fatal("cookie-mode order has no user_id")
	}
	eventsCh := h.openEvents(order.ID, cookie)

	// Checkout returns the stub simulate URL.
	resp := h.do(http.MethodPost, "/api/v1/orders/"+string(order.ID)+"/checkout", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkout status = %d, want 200", resp.StatusCode)
	}
	var redirect struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&redirect); err != nil {
		t.Fatalf("decode checkout: %v", err)
	}
	if redirect.RedirectURL != "/api/v1/orders/"+string(order.ID)+"/checkout/simulate" {
		t.Fatalf("redirect_url = %q", redirect.RedirectURL)
	}

	// The simulate endpoint drives MarkPaid, exactly the seam the Stripe webhook uses.
	noRedirect := *h.server.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	simulateReq, err := http.NewRequest(http.MethodGet, h.server.URL+redirect.RedirectURL, nil)
	if err != nil {
		t.Fatalf("new simulate request: %v", err)
	}
	simulateReq.AddCookie(cookie)
	simulateResp, err := noRedirect.Do(simulateReq)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	defer simulateResp.Body.Close()
	if simulateResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("simulate status = %d, want 303", simulateResp.StatusCode)
	}

	paid := h.waitOrder(order.ID, 2*time.Second, func(o domain.Order) bool {
		return o.PaidAt != nil
	})
	if paid.PaidAt == nil || paid.PaymentReference == nil || *paid.PaymentReference != "stub/"+string(order.ID) {
		t.Fatalf("paid order = %+v", paid)
	}

	// The scheduler (fake clock, opening hours) picks the paid order up and Runner.Run is
	// still the only dial path.
	final := h.waitOrder(order.ID, 5*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderFinished && o.Outcome != nil && o.Outcome.Kind == domain.OutcomeBooked
	})
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", final.Attempts)
	}
	if final.Outcome.BookedSlot == nil || !final.Outcome.BookedSlot.DateTime.Equal(bookedAt) {
		t.Errorf("booked slot = %+v", final.Outcome.BookedSlot)
	}
	if h.telephony.dialCount() != 1 {
		t.Errorf("dials = %d, want 1", h.telephony.dialCount())
	}
	if h.agent.runCount() != 1 {
		t.Errorf("agent runs = %d, want 1", h.agent.runCount())
	}

	got := collectStatuses(eventsCh, 3*time.Second,
		domain.OrderCreated, domain.OrderScheduled, domain.OrderDialing,
		domain.OrderRinging, domain.OrderInCall, domain.OrderFinished)
	want := []domain.OrderStatus{
		domain.OrderCreated, domain.OrderScheduled, domain.OrderDialing,
		domain.OrderRinging, domain.OrderInCall, domain.OrderFinished,
	}
	if !containsSequence(got, want) {
		t.Fatalf("SSE statuses = %v, want sequence %v", got, want)
	}
}

func TestM3IntegrationAuthOffKeepsC5Flow(t *testing.T) {
	h := newM3Harness(t, "off", []m2Script{{busy: true}}, nil)

	// Auth routes degrade to 501, but the C5 unauthenticated order API is unchanged.
	resp := h.do(http.MethodPost, "/api/v1/auth/register",
		`{"email":"off@example.test","password":"password123"}`, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("register auth-off status = %d, want 501", resp.StatusCode)
	}

	order := h.createOrder(nil)
	if order.UserID != "" {
		t.Errorf("auth-off order user_id = %q, want empty", order.UserID)
	}

	// The API-key place-call eligibility endpoint still works (no key configured => open).
	resp = h.do(http.MethodPost, "/api/v1/orders/"+string(order.ID)+"/call", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("place-call status = %d, want 202", resp.StatusCode)
	}
	scheduled := h.waitOrder(order.ID, 2*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderScheduled
	})
	if scheduled.NextAttemptAt == nil {
		t.Error("auth-off place-call did not schedule the order")
	}
}

// compile-time assertion that the hasher satisfies the port used by the composition root.
var _ ports.PasswordHasher = m3Hasher{}
