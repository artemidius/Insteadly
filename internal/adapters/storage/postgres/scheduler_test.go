//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"

	pgstore "github.com/artemidius/insteadly/internal/adapters/storage/postgres"
)

func makePaid(t *testing.T, store *pgstore.Store) domain.Order {
	t.Helper()
	order := createOrder(t, store, sampleOrder())
	paid, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
		return o.Transition(domain.OrderPaid)
	})
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	return paid
}

func makeScheduled(t *testing.T, store *pgstore.Store, next *time.Time) domain.Order {
	t.Helper()
	order := createOrder(t, store, sampleOrder())
	scheduled, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Schedule(next)
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return scheduled
}

func scheduledAt(now time.Time, offset time.Duration) *time.Time {
	at := now.Add(offset)
	return &at
}

func TestListEligibleFilters(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	paid := makePaid(t, store)
	due := makeScheduled(t, store, scheduledAt(now, -time.Hour))
	future := makeScheduled(t, store, scheduledAt(now, time.Hour))
	created := createOrder(t, store, sampleOrder())
	leased := makeScheduled(t, store, scheduledAt(now, -time.Hour))
	if _, err := store.Claim(context.Background(), leased.ID, "worker", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	list, err := store.ListEligible(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	want := []domain.OrderID{paid.ID, due.ID}
	if len(list) != len(want) {
		t.Fatalf("eligible = %d (%+v), want %d", len(list), list, len(want))
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("eligible[%d] = %s, want %s", i, list[i].ID, id)
		}
	}
	for _, o := range list {
		if o.ID == future.ID || o.ID == created.ID || o.ID == leased.ID {
			t.Errorf("ineligible order %s returned", o.ID)
		}
	}
}

func TestClaimContentionExactlyOneWins(t *testing.T) {
	store := newTestStore(t)
	order := makePaid(t, store)
	now := time.Now()

	const contenders = 8
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			_, err := store.Claim(context.Background(), order.ID, fmt.Sprintf("worker-%d", worker), now.Add(time.Minute))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	wins, misses := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ports.ErrNotEligible):
			misses++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if wins != 1 || misses != contenders-1 {
		t.Fatalf("wins=%d misses=%d, want 1/%d", wins, misses, contenders-1)
	}
	got, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.OrderPaid {
		t.Errorf("claim changed status to %q", got.Status)
	}
}

func TestListEligibleSkipsLockedRow(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	locked := makeScheduled(t, store, scheduledAt(now, -time.Hour))
	open := makeScheduled(t, store, scheduledAt(now, -time.Hour))

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM orders WHERE id = $1 FOR UPDATE`, string(locked.ID)).Scan(&id); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	list, err := store.ListEligible(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 1 || list[0].ID != open.ID {
		t.Fatalf("ListEligible = %+v, want only %s (SKIP LOCKED)", list, open.ID)
	}
}

func TestDisjointClaimsAcrossWorkers(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	const orders = 6
	ids := make(map[domain.OrderID]struct{}, orders)
	for i := 0; i < orders; i++ {
		o := makeScheduled(t, store, scheduledAt(now, -time.Hour))
		ids[o.ID] = struct{}{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan struct{})
	var mu sync.Mutex
	claimed := make(map[domain.OrderID]int, orders)
	errCh := make(chan error, 8)
	var wg sync.WaitGroup

	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", worker)
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				default:
				}
				candidates, err := store.ListEligible(ctx, now, 1)
				if err != nil {
					errCh <- err
					return
				}
				if len(candidates) == 0 {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				order, err := store.Claim(ctx, candidates[0].ID, owner, now.Add(time.Minute))
				if errors.Is(err, ports.ErrNotEligible) {
					continue
				}
				if err != nil {
					errCh <- err
					return
				}
				mu.Lock()
				claimed[order.ID]++
				if len(claimed) == orders && claimed[order.ID] == 1 {
					mu.Unlock()
					close(done)
					return
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("worker error: %v", err)
	}

	if len(claimed) != orders {
		t.Fatalf("claimed %d distinct orders, want %d: %+v", len(claimed), orders, claimed)
	}
	for id, count := range claimed {
		if count != 1 {
			t.Errorf("order %s claimed %d times", id, count)
		}
		if _, ok := ids[id]; !ok {
			t.Errorf("claim returned unknown order %s", id)
		}
	}
}

func TestExpiredLeaseIsReclaimable(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	order := makePaid(t, store)
	if _, err := store.Claim(context.Background(), order.ID, "worker-1", now.Add(-time.Second)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	list, err := store.ListEligible(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 1 || list[0].ID != order.ID {
		t.Fatalf("expired lease still blocks eligibility: %+v", list)
	}
	reclaimed, err := store.Claim(context.Background(), order.ID, "worker-2", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim after expiry: %v", err)
	}
	if reclaimed.ID != order.ID {
		t.Errorf("reclaimed = %s", reclaimed.ID)
	}
}

func TestReleaseOnlyOwnLease(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	order := makePaid(t, store)
	if _, err := store.Claim(context.Background(), order.ID, "worker-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Release(context.Background(), order.ID, "other"); err != nil {
		t.Fatalf("Release(other): %v", err)
	}
	if _, err := store.Claim(context.Background(), order.ID, "worker-2", now.Add(time.Minute)); !errors.Is(err, ports.ErrNotEligible) {
		t.Fatalf("foreign release cleared the lease: err = %v", err)
	}
	if err := store.Release(context.Background(), order.ID, "worker-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.Release(context.Background(), order.ID, "worker-1"); err != nil {
		t.Fatalf("idempotent Release: %v", err)
	}
	if err := store.Release(context.Background(), domain.NewOrderID(), "worker-1"); err != nil {
		t.Fatalf("Release(missing): %v", err)
	}
	if _, err := store.Claim(context.Background(), order.ID, "worker-3", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim after release: %v", err)
	}
}

func TestClaimIneligible(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	future := makeScheduled(t, store, scheduledAt(now, time.Hour))
	created := createOrder(t, store, sampleOrder())
	for name, id := range map[string]domain.OrderID{
		"missing": domain.NewOrderID(),
		"future":  future.ID,
		"created": created.ID,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Claim(context.Background(), id, "worker", now.Add(time.Minute)); !errors.Is(err, ports.ErrNotEligible) {
				t.Fatalf("Claim(%s) err = %v, want ErrNotEligible", name, err)
			}
		})
	}
}

func TestListEligibleLimit(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		makePaid(t, store)
	}
	list, err := store.ListEligible(context.Background(), now, 2)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len = %d, want 2", len(list))
	}
	empty, err := store.ListEligible(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("ListEligible(0): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("limit 0 returned %d orders", len(empty))
	}
}
