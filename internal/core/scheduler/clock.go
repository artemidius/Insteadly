package scheduler

import (
	"sync"
	"time"
)

// Clock is the scheduler's only source of time. Every policy decision (eligibility,
// window math, retry backoff, lease deadlines) reads it; the wall-clock ticker is the
// only other time consumer.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// FakeClock is a manually advanced clock for deterministic tests. It is safe for
// concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a fake clock pinned to now.
func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

// Now returns the pinned time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set pins the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
