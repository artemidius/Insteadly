package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Create persists a new order. It assigns an ID when empty, stamps missing
// timestamps, forces the status to "created" (a fresh record cannot be further along),
// and fails with ports.ErrConflict when the ID is already taken.
func (s *Store) Create(ctx context.Context, o domain.Order) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	path, err := s.orderPath(o.ID)
	if err != nil {
		return domain.Order{}, err
	}
	if _, err := os.Stat(path); err == nil {
		return domain.Order{}, fmt.Errorf("order %s: %w", o.ID, ports.ErrConflict)
	} else if !os.IsNotExist(err) {
		return domain.Order{}, fmt.Errorf("order %s: stat: %w", o.ID, err)
	}

	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = now
	}
	o.Status = domain.OrderCreated
	if err := s.writeOrderLocked(o); err != nil {
		return domain.Order{}, err
	}
	return o, nil
}

// Get returns the stored order, or an error wrapping ports.ErrNotFound.
func (s *Store) Get(ctx context.Context, id domain.OrderID) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	path, err := s.orderPath(id)
	if err != nil {
		// A malformed ID can never address a stored order; report it as a miss so the
		// API can answer 404 without leaking path-validation details.
		return domain.Order{}, fmt.Errorf("order %s: %w (%v)", id, ports.ErrNotFound, err)
	}
	o, err := readOrder(path)
	if errors.Is(err, ports.ErrNotFound) {
		return domain.Order{}, fmt.Errorf("order %s: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("order %s: %w", id, err)
	}
	return o, nil
}

// List returns every order, newest first by CreatedAt (ID breaks ties, keeping the
// output deterministic). A corrupt file is an error: loud corruption beats silent data
// loss.
func (s *Store) List(ctx context.Context) ([]domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := filepath.Join(s.dir, "orders")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	orders := make([]domain.Order, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		o, err := readOrder(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("list orders: %s: %w", e.Name(), err)
		}
		orders = append(orders, o)
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].CreatedAt.Equal(orders[j].CreatedAt) {
			return orders[i].ID > orders[j].ID
		}
		return orders[i].CreatedAt.After(orders[j].CreatedAt)
	})
	return orders, nil
}

// Update atomically applies fn to the stored order and persists the result. fn runs
// under the store's write lock: it must be fast, must not do I/O, and must not call
// back into the store. fn may not change the order ID.
func (s *Store) Update(ctx context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	if fn == nil {
		return domain.Order{}, fmt.Errorf("update order %s: nil update function", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.orderPath(id)
	if err != nil {
		// A malformed ID can never address a stored order; report it as a miss so the
		// API can answer 404 without leaking path-validation details.
		return domain.Order{}, fmt.Errorf("order %s: %w (%v)", id, ports.ErrNotFound, err)
	}
	o, err := readOrder(path)
	if errors.Is(err, ports.ErrNotFound) {
		return domain.Order{}, fmt.Errorf("order %s: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("order %s: %w", id, err)
	}

	if err := fn(&o); err != nil {
		return domain.Order{}, fmt.Errorf("update order %s: %w", id, err)
	}
	if o.ID != id {
		return domain.Order{}, fmt.Errorf("update order %s: update function changed id to %q", id, o.ID)
	}
	if err := s.writeOrderLocked(o); err != nil {
		return domain.Order{}, err
	}
	return o, nil
}

// writeOrderLocked persists o; the caller must hold s.mu.
func (s *Store) writeOrderLocked(o domain.Order) error {
	path, err := s.orderPath(o.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("order %s: encode: %w", o.ID, err)
	}
	if err := writeAtomic(path, 0o600, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("order %s: %w", o.ID, err)
	}
	return nil
}

// readOrder decodes one order file. A missing file returns ports.ErrNotFound; the
// caller adds the order ID for context.
func readOrder(path string) (domain.Order, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.Order{}, ports.ErrNotFound
		}
		return domain.Order{}, fmt.Errorf("read: %w", err)
	}
	var o domain.Order
	if err := json.Unmarshal(data, &o); err != nil {
		return domain.Order{}, fmt.Errorf("decode: %w", err)
	}
	return o, nil
}
