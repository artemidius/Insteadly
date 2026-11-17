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

// Create inserts a new order. It assigns an ID/timestamps when missing, forces status
// "created" (mirroring the local adapter), and reports ports.ErrConflict on duplicate
// IDs via ON CONFLICT DO NOTHING.
func (s *Store) Create(ctx context.Context, o domain.Order) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = now
	}
	o.Status = domain.OrderCreated
	enc, err := encodeOrder(o)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: create order %s: %w", o.ID, err)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO orders (
			id, status, clinic, patient, outcome, recording,
			created_at, updated_at, attempts, next_attempt_at, attempt_history,
			user_id, paid_at, payment_reference
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO NOTHING`,
		string(o.ID), string(o.Status), enc.clinic, enc.patient, enc.outcome, enc.recording,
		o.CreatedAt, o.UpdatedAt, o.Attempts, o.NextAttemptAt, enc.history,
		enc.userID, enc.paidAt, enc.paymentReference,
	)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: create order %s: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Order{}, fmt.Errorf("postgres: order %s: %w", o.ID, ports.ErrConflict)
	}
	return o, nil
}

// Get returns the stored order or an error wrapping ports.ErrNotFound.
func (s *Store) Get(ctx context.Context, id domain.OrderID) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	order, err := scanOrder(s.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE id = $1`, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("postgres: order %s: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: get order %s: %w", id, err)
	}
	return order, nil
}

// List returns every order newest first (created_at DESC, id DESC), mirroring the local
// adapter's tie-break.
func (s *Store) List(ctx context.Context) ([]domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+orderColumns+` FROM orders ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list orders: %w", err)
	}
	defer rows.Close()

	orders := make([]domain.Order, 0)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: list orders: %w", err)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list orders: %w", err)
	}
	return orders, nil
}

// Update applies fn under a row lock in one transaction. This is the same atomic
// Update(ctx,id,fn) contract the runner relies on, made cross-process-safe by SELECT …
// FOR UPDATE: the runner's scheduled→dialing check remains the duplicate-dial fence.
func (s *Store) Update(ctx context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return domain.Order{}, err
	}
	if fn == nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: nil update function", id)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	order, err := scanOrder(tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE id = $1 FOR UPDATE`, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("postgres: order %s: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: lock: %w", id, err)
	}
	if err := fn(&order); err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: %w", id, err)
	}
	if order.ID != id {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: update function changed id to %q", id, order.ID)
	}
	enc, err := encodeOrder(order)
	if err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE orders SET
			status = $2, clinic = $3, patient = $4, outcome = $5, recording = $6,
			created_at = $7, updated_at = $8, attempts = $9, next_attempt_at = $10,
			attempt_history = $11, user_id = $12, paid_at = $13, payment_reference = $14
		WHERE id = $1`,
		string(order.ID), string(order.Status), enc.clinic, enc.patient, enc.outcome, enc.recording,
		order.CreatedAt, order.UpdatedAt, order.Attempts, order.NextAttemptAt, enc.history,
		enc.userID, enc.paidAt, enc.paymentReference,
	); err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: write: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, fmt.Errorf("postgres: update order %s: commit: %w", id, err)
	}
	return order, nil
}
