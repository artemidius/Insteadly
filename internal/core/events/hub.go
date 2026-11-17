// Package events provides the in-process order-status hub the API's SSE endpoint
// subscribes to. It has no HTTP, database, or provider knowledge: the runner and the
// scheduler publish; the HTTP layer subscribes. A slow subscriber is dropped (its
// channel is closed) rather than blocking a publisher, because SSE clients reconnect
// and re-snapshot.
package events

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// DefaultBuffer is the per-subscriber channel capacity used when NewHub receives a
// non-positive buffer.
const DefaultBuffer = 64

// ErrClosed is returned by Subscribe after the hub has been closed.
var ErrClosed = errors.New("events: hub is closed")

// OrderEvent is one observed order status change. Status is the post-change status;
// Outcome is set only when the event is a finish (or the requeue clears it, signaled by
// status=scheduled with outcome=nil). IDs are domain IDs; no secrets or patient detail.
type OrderEvent struct {
	OrderID  domain.OrderID     `json:"order_id"`
	Status   domain.OrderStatus `json:"status"`
	Outcome  *domain.Outcome    `json:"outcome,omitempty"`
	Attempts int                `json:"attempts"`
	At       time.Time          `json:"at"`
}

// Publisher is what the runner and scheduler see. Publish is non-blocking: it drops on
// a full subscriber buffer, counts the drop, and returns nil. A nil or canceled context
// is ignored (a status event is not worth failing an attempt over).
type Publisher interface {
	Publish(ctx context.Context, ev OrderEvent) error
}

// Subscriber is what the SSE layer sees.
type Subscriber interface {
	Subscribe(ctx context.Context) (<-chan OrderEvent, func(), error)
}

// Hub is the concrete in-process fan-out. It is safe for concurrent use.
type Hub struct {
	buffer int

	mu      sync.Mutex
	closed  bool
	subs    map[*subscriber]struct{}
	dropped uint64
}

type subscriber struct {
	ch chan OrderEvent
}

// NewHub builds a hub with a bounded per-subscriber buffer (default DefaultBuffer).
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Hub{
		buffer: buffer,
		subs:   make(map[*subscriber]struct{}),
	}
}

// Publish fan-outs ev to every subscriber without blocking. A subscriber whose buffer
// is full is closed and removed; it is expected to reconnect and re-snapshot. A nil hub
// or a closed hub is a no-op so callers need no nil checks.
func (h *Hub) Publish(_ context.Context, ev OrderEvent) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			delete(h.subs, sub)
			close(sub.ch)
			h.dropped++
		}
	}
	return nil
}

// Subscribe registers a bounded channel and returns it with an idempotent unsubscribe
// function. Closing (or dropping) the channel is the only signal a subscriber needs.
func (h *Hub) Subscribe(ctx context.Context) (<-chan OrderEvent, func(), error) {
	if h == nil {
		return nil, nil, ErrClosed
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, ErrClosed
	}
	sub := &subscriber{ch: make(chan OrderEvent, h.buffer)}
	h.subs[sub] = struct{}{}

	var once sync.Once
	return sub.ch, func() { once.Do(func() { h.remove(sub) }) }, nil
}

// remove closes one subscription exactly once.
func (h *Hub) remove(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	close(sub.ch)
}

// Close stops accepting subscribers and closes every live subscription. It is
// idempotent.
func (h *Hub) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for sub := range h.subs {
		delete(h.subs, sub)
		close(sub.ch)
	}
	return nil
}

// Dropped reports how many slow subscribers have been dropped for overflow.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}
