package runner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// --- order store fake -----------------------------------------------------------

type fakeStore struct {
	mu           sync.Mutex
	orders       map[domain.OrderID]domain.Order
	history      []domain.Order
	updates      int
	beforeUpdate func(o *domain.Order)
	failOnFinish bool
	finishErr    error
	finishCount  int
}

func newFakeStore(orders ...domain.Order) *fakeStore {
	s := &fakeStore{orders: make(map[domain.OrderID]domain.Order, len(orders))}
	for _, o := range orders {
		s.orders[o.ID] = o
	}
	return s
}

func (s *fakeStore) Create(_ context.Context, o domain.Order) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	if _, exists := s.orders[o.ID]; exists {
		return domain.Order{}, ports.ErrConflict
	}
	o.Status = domain.OrderCreated
	s.orders[o.ID] = o
	return o, nil
}

func (s *fakeStore) Get(_ context.Context, id domain.OrderID) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	return o, nil
}

func (s *fakeStore) List(context.Context) ([]domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out, nil
}

func (s *fakeStore) Update(_ context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	if s.beforeUpdate != nil {
		s.beforeUpdate(&o)
	}
	if err := fn(&o); err != nil {
		return domain.Order{}, err
	}
	if s.failOnFinish && o.Status == domain.OrderFinished {
		if s.finishErr != nil {
			return domain.Order{}, s.finishErr
		}
		return domain.Order{}, fmt.Errorf("finalize failed")
	}
	if o.Status == domain.OrderFinished {
		s.finishCount++
	}
	s.orders[id] = o
	s.history = append(s.history, o)
	s.updates++
	return o, nil
}

func (s *fakeStore) statusHistory() []domain.OrderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.OrderStatus, len(s.history))
	for i, o := range s.history {
		out[i] = o.Status
	}
	return out
}

// --- call session fake ----------------------------------------------------------

type fakeSession struct {
	events  chan domain.CallEvent
	inbound chan domain.AudioChunk

	mu              sync.Mutex
	hangupCount     int
	hangupCtxErr    error
	sentAudio       int
	ended           bool
	autoEndOnHangup bool
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		events:          make(chan domain.CallEvent, 32),
		inbound:         make(chan domain.AudioChunk),
		autoEndOnHangup: true,
	}
}

func (s *fakeSession) ID() domain.CallID               { return "call-1" }
func (s *fakeSession) Events() <-chan domain.CallEvent { return s.events }
func (s *fakeSession) Inbound() <-chan domain.AudioChunk {
	return s.inbound
}

func (s *fakeSession) SendAudio(context.Context, domain.AudioChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sentAudio++
	return nil
}

func (s *fakeSession) SendDTMF(context.Context, string) error { return nil }

func (s *fakeSession) ClearAudio(context.Context) error { return nil }

func (s *fakeSession) MarkPlayback(context.Context, string) error { return nil }

func (s *fakeSession) Hangup(ctx context.Context) error {
	s.mu.Lock()
	s.hangupCount++
	s.hangupCtxErr = ctx.Err()
	auto := s.autoEndOnHangup
	s.mu.Unlock()
	if auto {
		s.end(domain.EndAborted)
	}
	return nil
}

func (s *fakeSession) end(reason domain.EndReason) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	s.events <- domain.CallEvent{CallID: "call-1", Kind: domain.EventEnded, EndReason: reason, At: time.Now()}
	close(s.events)
}

// closeWithoutEnd simulates a provider-contract violation: the channel closes with no
// terminal EventEnded.
func (s *fakeSession) closeWithoutEnd() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	close(s.events)
}

func (s *fakeSession) send(ev domain.CallEvent) {
	s.events <- ev
}

func (s *fakeSession) hangups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hangupCount
}

type readyWrapper struct {
	*fakeSession
	ready chan struct{}
}

func (r readyWrapper) StreamReady() <-chan struct{} { return r.ready }

// --- telephony fake -------------------------------------------------------------

type fakeTelephony struct {
	mu        sync.Mutex
	dialErr   error
	dialCount int
	session   *fakeSession
	ready     chan struct{}

	blockDial      chan struct{}
	sessionFactory func() *fakeSession
}

func (t *fakeTelephony) Dial(ctx context.Context, _ domain.Order) (ports.CallSession, error) {
	t.mu.Lock()
	t.dialCount++
	block := t.blockDial
	err := t.dialErr
	session := t.session
	if t.sessionFactory != nil {
		session = t.sessionFactory()
	}
	ready := t.ready
	t.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if ready != nil {
		return readyWrapper{fakeSession: session, ready: ready}, nil
	}
	return session, nil
}

func (t *fakeTelephony) dials() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dialCount
}

// --- agent fake -----------------------------------------------------------------

type fakeAgent struct {
	mu        sync.Mutex
	runs      int
	outcome   domain.Outcome
	err       error
	block     bool
	ignoreCtx bool
	release   chan struct{}
	started   chan struct{}
	onRun     func(ctx context.Context, s *fakeSession)
	session   *fakeSession
}

func newFakeAgent(outcome domain.Outcome, err error) *fakeAgent {
	return &fakeAgent{outcome: outcome, err: err}
}

func (a *fakeAgent) Run(ctx context.Context, _ domain.Order, _ agent.AudioSession, _ <-chan domain.CallEvent, _ <-chan domain.CallEvent) (domain.Outcome, error) {
	a.mu.Lock()
	a.runs++
	started := a.started
	a.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if a.onRun != nil {
		a.onRun(ctx, a.session)
	}
	if a.block {
		if a.ignoreCtx {
			<-a.release
			return a.outcome, a.err
		}
		<-ctx.Done()
		return domain.Outcome{}, ctx.Err()
	}
	return a.outcome, a.err
}

func (a *fakeAgent) runCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs
}

// --- fixtures -------------------------------------------------------------------

func testOrder() domain.Order {
	return domain.Order{
		ID:      domain.NewOrderID(),
		Status:  domain.OrderScheduled,
		Clinic:  domain.Clinic{Name: "Ordination", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max Mustermann"},
	}
}

func bookedOutcome() domain.Outcome {
	return domain.Outcome{
		Kind:   domain.OutcomeBooked,
		Detail: "booked",
		BookedSlot: &domain.BookedSlot{
			DateTime: time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600)),
		},
	}
}

func testRunner(t *testing.T, store *fakeStore, telephony *fakeTelephony, ag agent.Engine) *Runner {
	t.Helper()
	r, err := New(Config{
		Telephony:      telephony,
		Orders:         store,
		Agent:          ag,
		CleanupTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func progress(answered bool) []domain.CallEvent {
	events := []domain.CallEvent{
		{Kind: domain.EventDialing},
		{Kind: domain.EventRinging},
	}
	if answered {
		events = append(events, domain.CallEvent{Kind: domain.EventAnswered})
	}
	return events
}
