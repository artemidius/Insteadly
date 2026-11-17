package local

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func makePaid(t *testing.T, s *Store) domain.Order {
	t.Helper()
	o := mustCreate(t, s, sampleOrder())
	paid, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
		return order.Transition(domain.OrderPaid)
	})
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	return paid
}

func makeScheduled(t *testing.T, s *Store, next *time.Time) domain.Order {
	t.Helper()
	o := makePaid(t, s)
	scheduled, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
		return order.Schedule(next)
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return scheduled
}

func TestListEligibleSelectsAndSorts(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	dueEarly := now.Add(-2 * time.Hour)
	dueLater := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	paid := makePaid(t, s)
	scheduledEarly := makeScheduled(t, s, &dueEarly)
	scheduledLater := makeScheduled(t, s, &dueLater)
	notDue := makeScheduled(t, s, &future)
	created := mustCreate(t, s, sampleOrder())

	list, err := s.ListEligible(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	want := []domain.OrderID{paid.ID, scheduledEarly.ID, scheduledLater.ID}
	if len(list) != len(want) {
		t.Fatalf("eligible = %d orders, want %d: %+v", len(list), len(want), list)
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("eligible[%d] = %s, want %s", i, list[i].ID, id)
		}
	}
	for _, o := range list {
		if o.ID == notDue.ID || o.ID == created.ID {
			t.Errorf("ineligible order %s was returned", o.ID)
		}
	}
}

func TestListEligibleTieBreaksByID(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	same := now.Add(-time.Minute)
	a := makeScheduled(t, s, &same)
	b := makeScheduled(t, s, &same)

	list, err := s.ListEligible(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("eligible = %d, want 2", len(list))
	}
	if list[0].ID != a.ID && list[0].ID != b.ID {
		t.Fatalf("unexpected IDs: %+v", list)
	}
	if list[0].ID > list[1].ID {
		t.Errorf("IDs are not ascending: %s > %s", list[0].ID, list[1].ID)
	}
}

func TestListEligibleRespectsLimitAndZero(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	makePaid(t, s)
	makePaid(t, s)
	list, err := s.ListEligible(context.Background(), now, 1)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("len = %d, want 1", len(list))
	}
	empty, err := s.ListEligible(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("ListEligible(0): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("limit 0 returned %d orders", len(empty))
	}
}

func TestClaimLeasesWithoutChangingStatus(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	due := now.Add(-time.Minute)
	o := makeScheduled(t, s, &due)

	claimed, err := s.Claim(context.Background(), o.ID, "worker-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.ID != o.ID || claimed.Status != domain.OrderScheduled {
		t.Errorf("claimed = %+v, want the same scheduled order", claimed)
	}
	if _, err := s.Claim(context.Background(), o.ID, "worker-2", now.Add(time.Minute)); !errors.Is(err, ports.ErrNotEligible) {
		t.Fatalf("second Claim err = %v, want ErrNotEligible", err)
	}
	list, err := s.ListEligible(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("ListEligible: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("leased order is still eligible: %+v", list)
	}
}

func TestClaimPaidOrder(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	o := makePaid(t, s)
	claimed, err := s.Claim(context.Background(), o.ID, "worker", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.Status != domain.OrderPaid {
		t.Errorf("status = %q, want paid (claim never changes status)", claimed.Status)
	}
}

func TestExpiredLeaseIsReclaimable(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	o := makePaid(t, s)
	if _, err := s.Claim(context.Background(), o.ID, "worker-1", now.Add(-time.Second)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	reclaimed, err := s.Claim(context.Background(), o.ID, "worker-2", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim after expiry: %v", err)
	}
	if reclaimed.ID != o.ID {
		t.Errorf("reclaimed = %s, want %s", reclaimed.ID, o.ID)
	}
}

func TestReleaseClearsOnlyOwnLease(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	o := makePaid(t, s)
	if _, err := s.Claim(context.Background(), o.ID, "worker-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Release(context.Background(), o.ID, "other"); err != nil {
		t.Fatalf("Release(other): %v", err)
	}
	if _, err := s.Claim(context.Background(), o.ID, "worker-2", now.Add(time.Minute)); !errors.Is(err, ports.ErrNotEligible) {
		t.Fatalf("Claim after foreign release err = %v, want ErrNotEligible", err)
	}
	if err := s.Release(context.Background(), o.ID, "worker-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := s.Release(context.Background(), o.ID, "worker-1"); err != nil {
		t.Fatalf("idempotent Release: %v", err)
	}
	if _, err := s.Claim(context.Background(), o.ID, "worker-3", now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim after release: %v", err)
	}
}

func TestClaimIneligible(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	future := now.Add(time.Hour)
	futureOrder := makeScheduled(t, s, &future)
	created := mustCreate(t, s, sampleOrder())

	for name, id := range map[string]domain.OrderID{
		"missing":   domain.NewOrderID(),
		"future":    futureOrder.ID,
		"created":   created.ID,
		"malformed": "../../etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Claim(context.Background(), id, "worker", now.Add(time.Minute)); !errors.Is(err, ports.ErrNotEligible) {
				t.Fatalf("Claim(%s) err = %v, want ErrNotEligible", name, err)
			}
		})
	}
}

func TestClaimRequiresOwner(t *testing.T) {
	s := newStore(t)
	o := makePaid(t, s)
	if _, err := s.Claim(context.Background(), o.ID, "  ", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected error for blank owner")
	}
}
