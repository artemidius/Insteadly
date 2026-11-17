package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// The local store is also the single-process scheduler store (§3.7). The lease lives
// in memory only; eligibility and attempt history are persisted through the ordinary
// order JSON.
var _ ports.SchedulerStore = (*Store)(nil)

// ListEligible returns paid orders plus scheduled orders that are due, excluding
// orders leased by a live claim. The result is ordered by next_attempt_at (nil first)
// then ID, so repeated scans under a fixed clock are deterministic.
func (s *Store) ListEligible(ctx context.Context, now time.Time, limit int) ([]domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []domain.Order{}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := filepath.Join(s.dir, "orders")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list eligible orders: %w", err)
	}
	out := make([]domain.Order, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		o, err := readOrder(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("list eligible orders: %s: %w", e.Name(), err)
		}
		if !eligible(o, now) || s.leasedLocked(o.ID, now) {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].NextAttemptAt, out[j].NextAttemptAt
		switch {
		case a == nil && b == nil:
			return out[i].ID < out[j].ID
		case a == nil:
			return true
		case b == nil:
			return false
		case a.Equal(*b):
			return out[i].ID < out[j].ID
		default:
			return a.Before(*b)
		}
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Claim leases one eligible order without touching its status. Missing, not-due,
// already-leased, or non-eligible orders all return ports.ErrNotEligible.
func (s *Store) Claim(ctx context.Context, id domain.OrderID, owner string, leaseUntil time.Time) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	if strings.TrimSpace(owner) == "" {
		return domain.Order{}, fmt.Errorf("claim order %s: owner is required", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.orderPath(id)
	if err != nil {
		return domain.Order{}, fmt.Errorf("claim order %s: %w", id, ports.ErrNotEligible)
	}
	o, err := readOrder(path)
	if errors.Is(err, ports.ErrNotFound) {
		return domain.Order{}, fmt.Errorf("claim order %s: %w", id, ports.ErrNotEligible)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("claim order %s: %w", id, err)
	}
	now := time.Now()
	if !eligible(o, now) || s.leasedLocked(id, now) {
		return domain.Order{}, fmt.Errorf("claim order %s: %w", id, ports.ErrNotEligible)
	}
	s.leases[id] = lease{owner: owner, until: leaseUntil}
	return o, nil
}

// Release clears owner's lease, if any. It is idempotent and treats a missing order as
// already released.
func (s *Store) Release(ctx context.Context, id domain.OrderID, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[id]; ok && l.owner == owner {
		delete(s.leases, id)
	}
	return nil
}

// eligible reports whether o can be dialed now: paid is always eligible, scheduled is
// eligible once its next_attempt_at has passed (nil/zero = due immediately).
func eligible(o domain.Order, now time.Time) bool {
	switch o.Status {
	case domain.OrderPaid:
		return true
	case domain.OrderScheduled:
		return o.NextAttemptAt == nil || !o.NextAttemptAt.After(now)
	default:
		return false
	}
}

// leasedLocked reports whether id has a live lease at now. The caller must hold s.mu.
func (s *Store) leasedLocked(id domain.OrderID, now time.Time) bool {
	l, ok := s.leases[id]
	return ok && l.until.After(now)
}
