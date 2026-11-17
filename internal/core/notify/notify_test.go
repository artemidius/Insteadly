package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// --- fakes -----------------------------------------------------------------------

type fakeOrders struct {
	mu     sync.Mutex
	orders map[domain.OrderID]domain.Order
}

func newFakeOrders(orders ...domain.Order) *fakeOrders {
	f := &fakeOrders{orders: make(map[domain.OrderID]domain.Order)}
	for _, o := range orders {
		f.orders[o.ID] = o
	}
	return f
}

func (f *fakeOrders) Create(context.Context, domain.Order) (domain.Order, error) {
	return domain.Order{}, errors.New("not implemented")
}

func (f *fakeOrders) Get(_ context.Context, id domain.OrderID) (domain.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrders) List(context.Context) ([]domain.Order, error) { return nil, nil }

func (f *fakeOrders) Update(context.Context, domain.OrderID, func(*domain.Order) error) (domain.Order, error) {
	return domain.Order{}, errors.New("not implemented")
}

type fakeUsers struct {
	users map[domain.UserID]domain.User
}

func (f *fakeUsers) CreateUser(context.Context, string, string) (domain.User, error) {
	return domain.User{}, errors.New("not implemented")
}

func (f *fakeUsers) GetUserByEmail(context.Context, string) (domain.User, error) {
	return domain.User{}, ports.ErrNotFound
}

func (f *fakeUsers) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return domain.User{}, ports.ErrNotFound
	}
	return u, nil
}

func (f *fakeUsers) CreateSession(context.Context, domain.Session) error { return nil }

func (f *fakeUsers) GetSession(context.Context, string) (domain.Session, error) {
	return domain.Session{}, ports.ErrNotFound
}

func (f *fakeUsers) DeleteSession(context.Context, string) error { return nil }

type fakeNotifier struct {
	mu     sync.Mutex
	got    []domain.Notification
	err    error
	sendCh chan domain.Notification
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{sendCh: make(chan domain.Notification, 16)}
}

func (f *fakeNotifier) Send(_ context.Context, n domain.Notification) error {
	f.mu.Lock()
	f.got = append(f.got, n)
	err := f.err
	f.mu.Unlock()
	f.sendCh <- n
	return err
}

func (f *fakeNotifier) notifications() []domain.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Notification(nil), f.got...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func finishedEvent(order domain.Order, outcome *domain.Outcome, attempts int) events.OrderEvent {
	return events.OrderEvent{
		OrderID:  order.ID,
		Status:   domain.OrderFinished,
		Outcome:  outcome,
		Attempts: attempts,
		At:       time.Now(),
	}
}

func bookedOutcome() *domain.Outcome {
	return &domain.Outcome{
		Kind:   domain.OutcomeBooked,
		Detail: "ok",
		BookedSlot: &domain.BookedSlot{
			DateTime: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
			Doctor:   "Dr. Test",
			Practice: "Praxis Test",
		},
	}
}

func testOrder() domain.Order {
	return domain.Order{
		ID:      "0123456789abcdef0123456789abcdef",
		Status:  domain.OrderFinished,
		Clinic:  domain.Clinic{Name: "Praxis Test", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max Mustermann", Phone: "+436641234567"},
	}
}

type harness struct {
	hub      *events.Hub
	orders   *fakeOrders
	users    *fakeUsers
	notifier *fakeNotifier
	worker   *Worker
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
	runErr   error
}

func newHarness(t *testing.T, cfg func(*Config)) *harness {
	t.Helper()
	h := &harness{
		hub:      events.NewHub(64),
		orders:   newFakeOrders(testOrder()),
		users:    &fakeUsers{users: map[domain.UserID]domain.User{}},
		notifier: newFakeNotifier(),
		done:     make(chan error, 1),
	}
	c := Config{
		Events:   h.hub,
		Orders:   h.orders,
		Users:    h.users,
		Notifier: h.notifier,
		Logger:   testLogger(),
	}
	if cfg != nil {
		cfg(&c)
	}
	worker, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.worker = worker
	worker.started = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- worker.Run(ctx) }()
	select {
	case <-worker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not subscribe")
	}
	t.Cleanup(func() { h.stop(t) })
	return h
}

// stop cancels the worker and waits for it once, returning Run's error.
func (h *harness) stop(t *testing.T) error {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case h.runErr = <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return h.runErr
}

func (h *harness) publish(t *testing.T, ev events.OrderEvent) {
	t.Helper()
	if err := h.hub.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func (h *harness) waitNotification(t *testing.T) domain.Notification {
	t.Helper()
	select {
	case n := <-h.notifier.sendCh:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}
	return domain.Notification{}
}

func (h *harness) expectNoNotification(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case n := <-h.notifier.sendCh:
		t.Fatalf("unexpected notification: %+v", n)
	case <-time.After(wait):
	}
}

// --- tests -----------------------------------------------------------------------

func TestNewValidatesWiring(t *testing.T) {
	base := Config{
		Events:   events.NewHub(1),
		Orders:   newFakeOrders(),
		Notifier: newFakeNotifier(),
	}
	if _, err := New(base); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"no events":   func(c *Config) { c.Events = nil },
		"no orders":   func(c *Config) { c.Orders = nil },
		"no notifier": func(c *Config) { c.Notifier = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted %s", name)
			}
		})
	}
}

func TestFinishedWithOutcomeSendsOnce(t *testing.T) {
	h := newHarness(t, nil)
	order := testOrder()
	h.publish(t, finishedEvent(order, bookedOutcome(), 1))

	n := h.waitNotification(t)
	if n.OrderID != order.ID {
		t.Errorf("order id = %q", n.OrderID)
	}
	// No owning user + a patient phone => sms to the patient.
	if n.Channel != "sms" || n.To != "+436641234567" {
		t.Errorf("recipient = %q/%q, want sms/+436641234567", n.Channel, n.To)
	}
	if n.Subject != "Termin bestätigt" || !strings.Contains(n.Body, "20.11.2026 09:00") {
		t.Errorf("subject/body = %q / %q", n.Subject, n.Body)
	}
	h.expectNoNotification(t, 100*time.Millisecond)
	if got := len(h.notifier.notifications()); got != 1 {
		t.Fatalf("Send calls = %d, want 1", got)
	}
}

func TestNonTerminalEventsIgnored(t *testing.T) {
	h := newHarness(t, nil)
	order := testOrder()
	for _, status := range []domain.OrderStatus{
		domain.OrderCreated, domain.OrderPaid, domain.OrderScheduled,
		domain.OrderDialing, domain.OrderRinging, domain.OrderInCall,
	} {
		h.publish(t, events.OrderEvent{OrderID: order.ID, Status: status, At: time.Now()})
	}
	h.expectNoNotification(t, 100*time.Millisecond)

	// A finished event without an outcome is not actionable either.
	h.publish(t, finishedEvent(order, nil, 1))
	h.expectNoNotification(t, 100*time.Millisecond)

	// The worker is still alive and handles the real terminal event.
	h.publish(t, finishedEvent(order, bookedOutcome(), 1))
	h.waitNotification(t)
}

func TestDedupeByOrderAndAttempts(t *testing.T) {
	h := newHarness(t, nil)
	order := testOrder()
	ev := finishedEvent(order, bookedOutcome(), 1)
	h.publish(t, ev)
	h.waitNotification(t)

	h.publish(t, ev) // replayed publish
	h.expectNoNotification(t, 100*time.Millisecond)

	// A new attempt is a distinct notification.
	h.publish(t, finishedEvent(order, bookedOutcome(), 2))
	n := h.waitNotification(t)
	if n.OrderID != order.ID {
		t.Fatalf("notification = %+v", n)
	}
	if got := len(h.notifier.notifications()); got != 2 {
		t.Fatalf("Send calls = %d, want 2", got)
	}
}

func TestRecipientResolution(t *testing.T) {
	longOrder := testOrder()
	userID := domain.UserID("fedcba9876543210fedcba9876543210")
	longOrder.UserID = userID

	cases := []struct {
		name        string
		order       domain.Order
		users       *fakeUsers
		wantChannel string
		wantTo      string
	}{
		{
			name:        "owning user email",
			order:       longOrder,
			users:       &fakeUsers{users: map[domain.UserID]domain.User{userID: {ID: userID, Email: "patient@example.test"}}},
			wantChannel: "email",
			wantTo:      "patient@example.test",
		},
		{
			name:        "user unknown falls back to patient phone",
			order:       longOrder,
			users:       &fakeUsers{users: map[domain.UserID]domain.User{}},
			wantChannel: "sms",
			wantTo:      "+436641234567",
		},
		{
			name:        "no users store falls back to patient phone",
			order:       longOrder,
			users:       nil,
			wantChannel: "sms",
			wantTo:      "+436641234567",
		},
		{
			name:        "anonymous order with phone",
			order:       testOrder(),
			users:       &fakeUsers{users: map[domain.UserID]domain.User{}},
			wantChannel: "sms",
			wantTo:      "+436641234567",
		},
		{
			name:        "no recipient at all",
			order:       domain.Order{ID: testOrder().ID},
			users:       &fakeUsers{users: map[domain.UserID]domain.User{}},
			wantChannel: "email",
			wantTo:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				c.Orders = newFakeOrders(tc.order)
				if tc.users == nil {
					c.Users = nil
				} else {
					c.Users = tc.users
				}
			})
			h.publish(t, finishedEvent(tc.order, bookedOutcome(), 1))
			n := h.waitNotification(t)
			if n.Channel != tc.wantChannel || n.To != tc.wantTo {
				t.Fatalf("recipient = %q/%q, want %q/%q", n.Channel, n.To, tc.wantChannel, tc.wantTo)
			}
		})
	}
}

func TestSendErrorIsLoggedNotFatal(t *testing.T) {
	hub := events.NewHub(64)
	order := testOrder()
	en := &errorNotifier{attempts: make(chan struct{}, 8)}
	w, err := New(Config{
		Events:   hub,
		Orders:   newFakeOrders(order),
		Users:    &fakeUsers{users: map[domain.UserID]domain.User{}},
		Notifier: en,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	w.started = make(chan struct{})
	go func() { done <- w.Run(ctx) }()
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not subscribe")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	})

	if err := hub.Publish(context.Background(), finishedEvent(order, bookedOutcome(), 1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := hub.Publish(context.Background(), finishedEvent(order, bookedOutcome(), 2)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	count := 0
	timeout := time.After(2 * time.Second)
	for count < 2 {
		select {
		case <-en.attempts:
			count++
		case err := <-done:
			t.Fatalf("worker exited after %d attempt(s): %v", count, err)
		case <-timeout:
			t.Fatalf("send attempts = %d, want 2 (a failure must not stop the worker)", count)
		}
	}
}

type errorNotifier struct {
	attempts chan struct{}
}

func (e *errorNotifier) Send(context.Context, domain.Notification) error {
	select {
	case e.attempts <- struct{}{}:
	default:
	}
	return errors.New("delivery failed")
}

func TestSummaries(t *testing.T) {
	callback := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	cases := []struct {
		name        string
		outcome     *domain.Outcome
		wantSubject string
		wantBody    string
	}{
		{"booked", bookedOutcome(), "Termin bestätigt", "Dr. Test"},
		{"callback", &domain.Outcome{Kind: domain.OutcomeCallbackNeeded, Detail: "d", CallbackAt: &callback}, "Rückruf erforderlich", "20.11.2026 10:30"},
		{"failed", &domain.Outcome{Kind: domain.OutcomeFailed, Detail: "d", FailReason: "busy"}, "Anruf fehlgeschlagen", "busy"},
		{"aborted", &domain.Outcome{Kind: domain.OutcomeAborted, Detail: "d"}, "Anruf abgebrochen", "abgebrochen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, body := summarize(tc.outcome)
			if subject != tc.wantSubject || !strings.Contains(body, tc.wantBody) {
				t.Fatalf("summarize = %q / %q", subject, body)
			}
			if strings.Contains(body, "Max Mustermann") {
				t.Fatal("summary must not contain patient details")
			}
		})
	}
	if subject, _ := summarize(nil); subject == "" {
		t.Fatal("nil outcome must still produce a subject")
	}
}

func TestHubCloseStopsWorker(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.hub.Close(); err != nil {
		t.Fatalf("hub.Close: %v", err)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("Run = %v, want nil after hub close", err)
	}
}

func TestContextCancelStopsWorker(t *testing.T) {
	h := newHarness(t, nil)
	h.cancel()
	if err := h.stop(t); err != nil {
		t.Fatalf("Run = %v, want nil after cancel", err)
	}
}
