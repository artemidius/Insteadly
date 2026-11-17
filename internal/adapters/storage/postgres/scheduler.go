package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// ListEligible returns paid or due scheduled orders that no live lease holds. The query
// runs in a short transaction with FOR UPDATE SKIP LOCKED so concurrent listers skip rows
// another transaction is touching instead of serializing on them.
func (s *Store) ListEligible(ctx context.Context, now time.Time, limit int) ([]domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []domain.Order{}, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list eligible: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT `+orderColumns+` FROM orders
		WHERE status IN ('paid','scheduled')
		  AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		  AND (lease_until IS NULL OR lease_until <= $1)
		ORDER BY next_attempt_at NULLS FIRST, id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list eligible: %w", err)
	}
	orders := make([]domain.Order, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: list eligible: scan: %w", err)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("postgres: list eligible: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: list eligible: commit: %w", err)
	}
	return orders, nil
}

// Claim atomically leases one eligible order. The row is locked (SELECT … FOR UPDATE),
// eligibility is re-checked under the lock, and only the lease columns are written; the
// status is never changed. A missing, not-due, or already-leased order is ErrNotEligible.
func (s *Store) Claim(ctx context.Context, id domain.OrderID, owner string, leaseUntil time.Time) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	if owner == "" {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: owner is required", id)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	order, leaseUntilStored, err := scanOrderWithLease(tx.QueryRow(ctx,
		`SELECT `+orderColumns+`, lease_until FROM orders WHERE id = $1 FOR UPDATE`, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: %w", id, ports.ErrNotEligible)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: lock: %w", id, err)
	}
	now := time.Now()
	if !eligible(order, now) {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: %w", id, ports.ErrNotEligible)
	}
	if leaseUntilStored != nil && leaseUntilStored.After(now) {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: %w", id, ports.ErrNotEligible)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE orders SET lease_owner = $2, lease_until = $3 WHERE id = $1`,
		string(id), owner, leaseUntil)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: write: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: %w", id, ports.ErrNotEligible)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, fmt.Errorf("postgres: claim order %s: commit: %w", id, err)
	}
	return order, nil
}

// Release clears owner's lease. It is idempotent and never errors for a missing or
// already-released order.
func (s *Store) Release(ctx context.Context, id domain.OrderID, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE orders SET lease_owner = NULL, lease_until = NULL WHERE id = $1 AND lease_owner = $2`,
		string(id), owner); err != nil {
		return fmt.Errorf("postgres: release order %s: %w", id, err)
	}
	return nil
}

// eligible mirrors the local adapter and the ports.SchedulerStore contract.
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
