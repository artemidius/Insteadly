package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/recorder"
)

type fakeRecorder struct {
	mu     sync.Mutex
	wraps  int
	orders []domain.Order
	inner  ports.CallSession
}

func (f *fakeRecorder) Wrap(session ports.CallSession, order domain.Order) ports.CallSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wraps++
	f.orders = append(f.orders, order)
	f.inner = session
	return session
}

func (f *fakeRecorder) counts() (int, []domain.Order) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wraps, append([]domain.Order(nil), f.orders...)
}

// captureAgent records the session the runner actually handed to the agent.
type captureAgent struct {
	mu      sync.Mutex
	call    agent.AudioSession
	outcome domain.Outcome
}

func (a *captureAgent) Run(_ context.Context, _ domain.Order, call agent.AudioSession, _, _ <-chan domain.CallEvent) (domain.Outcome, error) {
	a.mu.Lock()
	a.call = call
	a.mu.Unlock()
	return a.outcome, nil
}

func (a *captureAgent) session() agent.AudioSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.call
}

func answeredRun(t *testing.T, rec recorder.Recorder) (*captureAgent, *fakeSession, *fakeRecorder, domain.Order) {
	t.Helper()
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session,
		domain.CallEvent{Kind: domain.EventDialing},
		domain.CallEvent{Kind: domain.EventRinging},
		domain.CallEvent{Kind: domain.EventAnswered},
	)
	telephony := &fakeTelephony{session: session}
	ag := &captureAgent{outcome: bookedOutcome()}
	spy := &fakeRecorder{}
	if rec == nil {
		rec = spy
	}
	r, err := New(Config{
		Telephony:      telephony,
		Orders:         store,
		Agent:          ag,
		Recorder:       rec,
		CleanupTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return ag, session, spy, order
}

func TestRecorderWrapAppliedExactlyOnceWithClaimedOrder(t *testing.T) {
	ag, session, spy, order := answeredRun(t, nil)

	wraps, orders := spy.counts()
	if wraps != 1 {
		t.Fatalf("Wrap calls = %d, want exactly 1", wraps)
	}
	if len(orders) != 1 {
		t.Fatalf("wrapped orders = %d", len(orders))
	}
	claimed := orders[0]
	if claimed.ID != order.ID {
		t.Errorf("wrapped order id = %q, want %q", claimed.ID, order.ID)
	}
	if claimed.Attempts != 1 || claimed.Status != domain.OrderDialing {
		t.Errorf("wrapped order = status %q attempts %d, want the claimed dialing order", claimed.Status, claimed.Attempts)
	}
	if ag.session() == nil {
		t.Fatal("agent received no session")
	}
	got, ok := ag.session().(ports.CallSession)
	if !ok {
		t.Fatalf("agent session %T is not a ports.CallSession", ag.session())
	}
	if got != ports.CallSession(session) {
		t.Fatalf("agent session = %p, want the dialed %p", got, session)
	}
}

func TestNilRecorderLeavesSessionUnchanged(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	preload(session,
		domain.CallEvent{Kind: domain.EventDialing},
		domain.CallEvent{Kind: domain.EventRinging},
		domain.CallEvent{Kind: domain.EventAnswered},
	)
	ag := &captureAgent{outcome: bookedOutcome()}
	r, err := New(Config{
		Telephony:      &fakeTelephony{session: session},
		Orders:         store,
		Agent:          ag,
		CleanupTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Run(context.Background(), order.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := ag.session().(ports.CallSession)
	if !ok {
		t.Fatalf("agent session %T is not a ports.CallSession", ag.session())
	}
	if got != ports.CallSession(session) {
		t.Fatalf("nil recorder changed the session: %p vs %p", got, session)
	}
}

func TestRecorderFlagsOffLeavesSessionIdentical(t *testing.T) {
	// The real recorder with every flag off must return the identical session.
	rec := recorder.New(nil, nil, recorder.Policy{}, nil)
	ag, session, _, _ := answeredRun(t, rec)
	got, ok := ag.session().(ports.CallSession)
	if !ok {
		t.Fatalf("agent session %T is not a ports.CallSession", ag.session())
	}
	if got != ports.CallSession(session) {
		t.Fatal("flags-off recorder changed the session")
	}
}
