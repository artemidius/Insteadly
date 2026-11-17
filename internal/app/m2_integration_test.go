package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
	"github.com/artemidius/insteadly/internal/core/scheduler"
	"github.com/artemidius/insteadly/internal/httpapi"
)

const m2OrderJSON = `{
  "clinic": {"name": "Praxis M2", "phone": "+431234567"},
  "patient": {"full_name": "Synthetic M2 Patient", "phone": "+436641234567", "reason": "Kontrolle"}
}`

// m2Script is one scripted telephony attempt: an immediate busy end, or an answered call
// whose dialogue outcome comes from the scripted agent.
type m2Script struct {
	answer bool
	busy   bool
}

type m2Telephony struct {
	mu      sync.Mutex
	scripts []m2Script
	dials   int
}

func (m *m2Telephony) Dial(context.Context, domain.Order) (ports.CallSession, error) {
	m.mu.Lock()
	if m.dials >= len(m.scripts) {
		attempt := m.dials
		m.mu.Unlock()
		return nil, fmt.Errorf("unexpected dial %d", attempt)
	}
	script := m.scripts[m.dials]
	m.dials++
	m.mu.Unlock()

	session := newIntegrationSession()
	session.events <- domain.CallEvent{CallID: "call-m2", Kind: domain.EventDialing, At: time.Now()}
	session.events <- domain.CallEvent{CallID: "call-m2", Kind: domain.EventRinging, At: time.Now()}
	if script.answer {
		session.events <- domain.CallEvent{CallID: "call-m2", Kind: domain.EventAnswered, At: time.Now()}
	}
	if script.busy {
		session.end(domain.EndBusy)
	}
	return readyIntegrationSession{session: session}, nil
}

func (m *m2Telephony) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

type m2Agent struct {
	mu       sync.Mutex
	outcomes []domain.Outcome
	runs     int
}

func (a *m2Agent) Run(context.Context, domain.Order, agent.AudioSession, <-chan domain.CallEvent, <-chan domain.CallEvent) (domain.Outcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs >= len(a.outcomes) {
		return domain.Outcome{}, errors.New("unexpected agent run")
	}
	outcome := a.outcomes[a.runs]
	a.runs++
	return outcome, nil
}

func (a *m2Agent) runCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs
}

type m2Harness struct {
	t         *testing.T
	store     *local.Store
	clock     *scheduler.FakeClock
	hub       *events.Hub
	app       *app.App
	runner    *runner.Runner
	scheduler *scheduler.Scheduler
	telephony *m2Telephony
	agent     *m2Agent
	server    *httptest.Server
	baseURL   string
	logs      *strings.Builder
	lastID    domain.OrderID
}

func m2TestConfig() config.Config {
	return config.Config{
		HTTP:      config.HTTP{Addr: "127.0.0.1:0"},
		Telephony: config.Telephony{Provider: "stub"},
		STT:       config.STT{Provider: "stub"},
		TTS:       config.TTS{Provider: "stub"},
		Dialogue:  config.Dialogue{Provider: "stub"},
		Storage:   config.Storage{Provider: "local"},
		Scheduler: config.Scheduler{
			Enabled:         true,
			Tick:            2 * time.Millisecond,
			MaxAttempts:     3,
			MaxConcurrent:   4,
			Lease:           time.Minute,
			BusyBackoff:     15 * time.Minute,
			NoAnswerBackoff: 30 * time.Minute,
			ErrorBackoff:    5 * time.Minute,
			WindowsJSON:     config.DefaultSchedulerWindows,
		},
		API: config.API{IdempotencyTTL: time.Minute},
		Log: config.Log{Level: "error", Format: "text"},
	}
}

func newM2Harness(t *testing.T, scripts []m2Script, outcomes []domain.Outcome) *m2Harness {
	t.Helper()
	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
		WorkerID:        "m2-test",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	cfg := m2TestConfig()
	application := &app.App{
		Config:    cfg,
		Storage:   store,
		Telephony: telephony,
		Log:       logger,
		Runner:    run,
		Scheduler: sched,
		Events:    hub,
	}
	application.Start(context.Background())
	server := httptest.NewServer(httpapi.New(cfg, application, "m2-test").Handler)
	t.Cleanup(func() {
		// Close the app first: it closes the scheduler, runner, and event hub, which ends
		// any active SSE handler so server.Close cannot block on it.
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
	return &m2Harness{
		t: t, store: store, clock: clock, hub: hub, app: application, runner: run,
		scheduler: sched, telephony: telephony, agent: fakeAgent, server: server,
		baseURL: server.URL, logs: logs,
	}
}

func mustWindows(t *testing.T) scheduler.Windows {
	t.Helper()
	windows, err := scheduler.ParseWindows(config.DefaultSchedulerWindows)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	return windows
}

func (h *m2Harness) post(path, body string, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *m2Harness) createOrder() domain.Order {
	h.t.Helper()
	resp := h.post("/api/v1/orders", m2OrderJSON, nil)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create status = %d", resp.StatusCode)
	}
	var order domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&order); err != nil {
		h.t.Fatalf("decode order: %v", err)
	}
	return order
}

// placeCall makes the order eligible through the real endpoint, then aligns the due time
// with the fake clock. The alignment is a test-clock concern only: the real endpoint uses
// the wall clock, while this scheduler runs at a pinned fake time.
func (h *m2Harness) placeCall(order domain.Order) domain.Order {
	h.t.Helper()
	resp := h.post("/api/v1/orders/"+string(order.ID)+"/call", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("place-call status = %d", resp.StatusCode)
	}
	var scheduled domain.Order
	if err := json.NewDecoder(resp.Body).Decode(&scheduled); err != nil {
		h.t.Fatalf("decode scheduled order: %v", err)
	}
	at := h.clock.Now()
	aligned, err := h.store.Orders().Update(context.Background(), order.ID, func(o *domain.Order) error {
		if o.Status != domain.OrderScheduled {
			return fmt.Errorf("order is %q, want scheduled", o.Status)
		}
		o.NextAttemptAt = &at
		return nil
	})
	if err != nil {
		h.t.Fatalf("align due time: %v", err)
	}
	return aligned
}

func (h *m2Harness) waitOrder(timeout time.Duration, pred func(domain.Order) bool) domain.Order {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		order, err := h.store.Orders().Get(context.Background(), h.lastID)
		if err == nil && pred(order) {
			return order
		}
		time.Sleep(2 * time.Millisecond)
	}
	order, err := h.store.Orders().Get(context.Background(), h.lastID)
	h.t.Fatalf("timed out waiting for order state; last = %+v err = %v", order, err)
	return domain.Order{}
}

type m2Event struct {
	name   string
	data   string
	status domain.OrderStatus
}

func (h *m2Harness) openEvents(id domain.OrderID) <-chan m2Event {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/api/v1/orders/"+string(id)+"/events", nil)
	if err != nil {
		cancel()
		h.t.Fatalf("new SSE request: %v", err)
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

func collectStatuses(ch <-chan m2Event, timeout time.Duration, want ...domain.OrderStatus) []domain.OrderStatus {
	deadline := time.After(timeout)
	var got []domain.OrderStatus
	for len(got) < len(want) {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev.status)
		case <-deadline:
			return got
		}
	}
	return got
}

func containsSequence(got, want []domain.OrderStatus) bool {
	i := 0
	for _, status := range got {
		if i < len(want) && status == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestM2IntegrationBusyRetryThenBooked(t *testing.T) {
	callbackAt := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	outcomes := []domain.Outcome{{
		Kind:       domain.OutcomeBooked,
		BookedSlot: &domain.BookedSlot{DateTime: callbackAt.Add(24 * time.Hour)},
	}}
	h := newM2Harness(t, []m2Script{{busy: true}, {answer: true}}, outcomes)
	order := h.createOrder()
	h.lastID = order.ID
	eventsCh := h.openEvents(order.ID)
	h.placeCall(order)

	requeued := h.waitOrder(5*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderScheduled && o.Outcome == nil && len(o.AttemptHistory) == 1
	})
	if requeued.Attempts != 1 {
		t.Fatalf("attempts after busy = %d, want 1", requeued.Attempts)
	}
	if got := requeued.AttemptHistory[0].Outcome; got.Kind != domain.OutcomeFailed || got.FailReason != "busy" {
		t.Fatalf("history outcome = %+v, want failed/busy", got)
	}
	wantNext := h.clock.Now().Add(15 * time.Minute)
	if requeued.NextAttemptAt == nil || !requeued.NextAttemptAt.Equal(wantNext) {
		t.Fatalf("next_attempt_at = %v, want %v", requeued.NextAttemptAt, wantNext)
	}

	h.clock.Advance(16 * time.Minute)
	final := h.waitOrder(5*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderFinished && o.Outcome != nil && o.Outcome.Kind == domain.OutcomeBooked
	})
	if final.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", final.Attempts)
	}
	if len(final.AttemptHistory) != 1 || final.AttemptHistory[0].Outcome.FailReason != "busy" {
		t.Errorf("history = %+v, want the preserved busy attempt", final.AttemptHistory)
	}
	if h.telephony.dialCount() != 2 {
		t.Errorf("dials = %d, want 2", h.telephony.dialCount())
	}
	if h.agent.runCount() != 1 {
		t.Errorf("agent runs = %d, want 1 (the busy attempt never answers)", h.agent.runCount())
	}

	got := collectStatuses(eventsCh, 3*time.Second,
		domain.OrderCreated, domain.OrderDialing, domain.OrderRinging, domain.OrderFinished,
		domain.OrderScheduled, domain.OrderDialing, domain.OrderRinging, domain.OrderInCall, domain.OrderFinished)
	want := []domain.OrderStatus{
		domain.OrderCreated, domain.OrderDialing, domain.OrderRinging, domain.OrderFinished,
		domain.OrderScheduled, domain.OrderDialing, domain.OrderRinging, domain.OrderInCall, domain.OrderFinished,
	}
	if !containsSequence(got, want) {
		t.Fatalf("SSE statuses = %v, want sequence %v", got, want)
	}
}

func TestM2IntegrationCallbackRequeue(t *testing.T) {
	callbackAt := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC) // fake now + 1h
	outcomes := []domain.Outcome{
		{Kind: domain.OutcomeCallbackNeeded, CallbackAt: &callbackAt},
		{Kind: domain.OutcomeBooked, BookedSlot: &domain.BookedSlot{DateTime: callbackAt.Add(24 * time.Hour)}},
	}
	h := newM2Harness(t, []m2Script{{answer: true}, {answer: true}}, outcomes)
	order := h.createOrder()
	h.lastID = order.ID
	h.placeCall(order)

	requeued := h.waitOrder(5*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderScheduled && o.Outcome == nil && len(o.AttemptHistory) == 1
	})
	if requeued.NextAttemptAt == nil || !requeued.NextAttemptAt.Equal(callbackAt) {
		t.Fatalf("next_attempt_at = %v, want callback_at %v", requeued.NextAttemptAt, callbackAt)
	}
	if got := requeued.AttemptHistory[0].Outcome; got.Kind != domain.OutcomeCallbackNeeded {
		t.Fatalf("history outcome = %+v, want callback_needed", got)
	}

	h.clock.Advance(61 * time.Minute)
	final := h.waitOrder(5*time.Second, func(o domain.Order) bool {
		return o.Status == domain.OrderFinished && o.Outcome != nil && o.Outcome.Kind == domain.OutcomeBooked
	})
	if final.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", final.Attempts)
	}
	if len(final.AttemptHistory) != 1 || final.AttemptHistory[0].Outcome.Kind != domain.OutcomeCallbackNeeded {
		t.Errorf("history = %+v, want the preserved callback attempt", final.AttemptHistory)
	}
}

func TestM2IntegrationPlaceCallIsEligibilityOnly(t *testing.T) {
	outcomes := []domain.Outcome{{
		Kind:       domain.OutcomeBooked,
		BookedSlot: &domain.BookedSlot{DateTime: time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)},
	}}
	h := newM2Harness(t, []m2Script{{answer: true}}, outcomes)
	order := h.createOrder()
	h.lastID = order.ID

	// The place-call endpoint itself never dials: the order becomes scheduled, and with
	// the pinned scheduler clock it stays ineligible, so no session is dialed.
	resp := h.post("/api/v1/orders/"+string(order.ID)+"/call", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("place-call status = %d", resp.StatusCode)
	}
	time.Sleep(30 * time.Millisecond)
	if h.telephony.dialCount() != 0 {
		t.Errorf("place-call dialed synchronously: %d dials", h.telephony.dialCount())
	}
	scheduled, err := h.store.Orders().Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if scheduled.Status != domain.OrderScheduled {
		t.Errorf("status = %q, want scheduled", scheduled.Status)
	}
}
