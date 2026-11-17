package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func testEvent(status domain.OrderStatus) OrderEvent {
	return OrderEvent{
		OrderID:  domain.NewOrderID(),
		Status:   status,
		Attempts: 1,
		At:       time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC),
	}
}

func TestPublishSubscribe(t *testing.T) {
	h := NewHub(4)
	defer h.Close()

	ch, unsub, err := h.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	ev := testEvent(domain.OrderDialing)
	if err := h.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case got := <-ch:
		if got.OrderID != ev.OrderID || got.Status != ev.Status || got.Attempts != ev.Attempts {
			t.Fatalf("event = %+v, want %+v", got, ev)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

func TestPublishReachesEverySubscriber(t *testing.T) {
	h := NewHub(2)
	defer h.Close()

	a, unsubA, _ := h.Subscribe(context.Background())
	b, unsubB, _ := h.Subscribe(context.Background())
	defer unsubA()
	defer unsubB()

	ev := testEvent(domain.OrderFinished)
	if err := h.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for name, ch := range map[string]<-chan OrderEvent{"a": a, "b": b} {
		select {
		case got := <-ch:
			if got.OrderID != ev.OrderID {
				t.Errorf("%s: event = %+v", name, got)
			}
		case <-time.After(time.Second):
			t.Errorf("%s: event not delivered", name)
		}
	}
}

func TestSlowSubscriberIsDroppedAndCounted(t *testing.T) {
	h := NewHub(1)
	defer h.Close()

	ch, _, err := h.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Nothing is read, so the second publish overflows the buffer.
	if err := h.Publish(context.Background(), testEvent(domain.OrderDialing)); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := h.Publish(context.Background(), testEvent(domain.OrderRinging)); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if got := h.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}

	got, ok := <-ch
	if !ok {
		t.Fatal("subscriber channel closed before the buffered event was read")
	}
	if got.Status != domain.OrderDialing {
		t.Errorf("buffered event = %+v", got)
	}
	if _, ok := <-ch; ok {
		t.Error("overflowed subscriber channel is still open")
	}
	// A dropped subscription no longer receives events.
	if err := h.Publish(context.Background(), testEvent(domain.OrderInCall)); err != nil {
		t.Fatalf("publish after drop: %v", err)
	}
	if got := h.Dropped(); got != 1 {
		t.Errorf("dropped = %d after publish to no subscribers, want 1", got)
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	h := NewHub(1)
	ch, _, err := h.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := <-ch; ok {
		t.Error("subscription channel is still open after Close")
	}
	if _, _, err := h.Subscribe(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close: err = %v, want ErrClosed", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Publishing to a closed hub is a no-op, not a panic.
	if err := h.Publish(context.Background(), testEvent(domain.OrderFinished)); err != nil {
		t.Fatalf("Publish after Close: %v", err)
	}
}

func TestNilContextPublishIsNoop(t *testing.T) {
	h := NewHub(1)
	defer h.Close()

	ch, _, err := h.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ev := testEvent(domain.OrderScheduled)
	var nilCtx context.Context // deliberate nil-context coverage for the nil-safe publisher contract
	if err := h.Publish(nilCtx, ev); err != nil {
		t.Fatalf("Publish(nil): %v", err)
	}
	select {
	case got := <-ch:
		if got.OrderID != ev.OrderID {
			t.Fatalf("event = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nil-context publish was not delivered")
	}

	var nilHub *Hub
	if err := nilHub.Publish(context.Background(), ev); err != nil {
		t.Fatalf("nil hub Publish: %v", err)
	}
	if err := nilHub.Close(); err != nil {
		t.Fatalf("nil hub Close: %v", err)
	}
}

func TestUnsubscribeIsIdempotentAndStopsDelivery(t *testing.T) {
	h := NewHub(2)
	defer h.Close()

	ch, unsub, err := h.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	unsub()
	unsub() // must not panic or double-close
	if _, ok := <-ch; ok {
		t.Error("unsubscribed channel is still open")
	}
	if err := h.Publish(context.Background(), testEvent(domain.OrderRinging)); err != nil {
		t.Fatalf("Publish after unsubscribe: %v", err)
	}
}

func TestSubscribeRejectsCanceledContext(t *testing.T) {
	h := NewHub(1)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := h.Subscribe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
