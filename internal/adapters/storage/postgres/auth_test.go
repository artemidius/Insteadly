//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestAuthUsersAndSessionsRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "  User@Example.TEST ", "$2a$12$hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != "user@example.test" {
		t.Errorf("email = %q, want lowercase", user.Email)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(string(user.ID)) {
		t.Errorf("user id = %q, want 32-hex", user.ID)
	}
	if user.PasswordHash != "$2a$12$hashed" || user.CreatedAt.IsZero() {
		t.Errorf("user = %+v", user)
	}

	byEmail, err := store.GetUserByEmail(ctx, "USER@example.test")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if byEmail.ID != user.ID || byEmail.PasswordHash != user.PasswordHash {
		t.Errorf("by email = %+v, want %+v", byEmail, user)
	}
	byID, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if byID.Email != user.Email {
		t.Errorf("by id = %+v", byID)
	}

	session := domain.Session{
		TokenHash: strings.Repeat("a", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := store.GetSession(ctx, session.TokenHash)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != user.ID {
		t.Errorf("session user = %q, want %q", got.UserID, user.ID)
	}
	if err := store.DeleteSession(ctx, session.TokenHash); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := store.GetSession(ctx, session.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
	if err := store.DeleteSession(ctx, "missing"); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
}

func TestAuthDuplicateEmailIsConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.CreateUser(ctx, "dup@example.test", "hash-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err := store.CreateUser(ctx, "DUP@example.test", "hash-2")
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAuthExpiredSessionIsNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "expired@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	session := domain.Session{
		TokenHash: strings.Repeat("b", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(-time.Minute).UTC(),
		CreatedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := store.GetSession(ctx, session.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("expired session err = %v, want ErrNotFound", err)
	}
}

func TestAuthDeletingUserCascadesSessions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "cascade@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	session := domain.Session{
		TokenHash: strings.Repeat("c", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	conn, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, string(user.ID)); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := store.GetSession(ctx, session.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("session survived the user delete, err = %v", err)
	}
}

func TestMigrationIdempotenceAndVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	// newTestStore already migrated; a second run must be a no-op.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version IN (1,2)`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 2 {
		t.Errorf("applied migrations = %d, want 2", count)
	}
	for _, column := range []string{"user_id", "paid_at", "payment_reference"} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'orders' AND column_name = $1
			)`, column).Scan(&exists); err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}
		if !exists {
			t.Errorf("orders.%s column is missing", column)
		}
	}
}

func TestOrderOwnershipAndPaymentReference(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user, err := store.CreateUser(ctx, "owner@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	paidAt := time.Now().UTC().Truncate(time.Microsecond)
	reference := "cs_test_1"
	created, err := store.Create(ctx, domain.Order{
		UserID:  user.ID,
		Clinic:  domain.Clinic{Name: "Praxis Test", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max Mustermann"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Update(ctx, created.ID, func(o *domain.Order) error {
		return o.MarkPaid(reference, paidAt)
	}); err != nil {
		t.Fatalf("MarkPaid Update: %v", err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != user.ID {
		t.Errorf("user_id = %q, want %q", got.UserID, user.ID)
	}
	if got.PaidAt == nil || !got.PaidAt.Equal(paidAt) {
		t.Errorf("paid_at = %v, want %v", got.PaidAt, paidAt)
	}
	if got.PaymentReference == nil || *got.PaymentReference != reference {
		t.Errorf("payment_reference = %v, want %q", got.PaymentReference, reference)
	}

	// One payment reference may mark at most one order paid (durable webhook idempotency).
	duplicate := domain.Order{
		PaymentReference: &reference,
		Clinic:           domain.Clinic{Name: "Other", Phone: "+431"},
		Patient:          domain.Patient{FullName: "Other Patient"},
	}
	if _, err := store.Create(ctx, duplicate); err == nil {
		t.Fatal("second order with the same payment_reference was accepted")
	} else if !strings.Contains(err.Error(), "orders_payment_reference_uniq") {
		t.Errorf("err = %v, want the payment_reference unique index violation", err)
	}

	// The FK rejects an order for a nonexistent user.
	unknownUser := domain.UserID(domain.NewOrderID())
	if _, err := store.Create(ctx, domain.Order{
		UserID:  unknownUser,
		Clinic:  domain.Clinic{Name: "FK", Phone: "+431"},
		Patient: domain.Patient{FullName: "FK"},
	}); err == nil {
		t.Error("order with an unknown user_id was accepted")
	}
}

func TestUpdatePersistsMarkPaid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	created, err := store.Create(ctx, domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis", Phone: "+431"},
		Patient: domain.Patient{FullName: "Max"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	when := time.Now().UTC().Truncate(time.Microsecond)
	updated, err := store.Update(ctx, created.ID, func(o *domain.Order) error {
		return o.MarkPaid("cs_update_1", when)
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != domain.OrderPaid || updated.PaidAt == nil || !updated.PaidAt.Equal(when) {
		t.Errorf("updated = %+v", updated)
	}
	if updated.PaymentReference == nil || *updated.PaymentReference != "cs_update_1" {
		t.Errorf("payment_reference = %v", updated.PaymentReference)
	}

	// A replayed MarkPaid through Update is a no-op and does not collide with the index.
	replay, err := store.Update(ctx, created.ID, func(o *domain.Order) error {
		return o.MarkPaid("cs_update_2", time.Now())
	})
	if err != nil {
		t.Fatalf("replay Update: %v", err)
	}
	if replay.PaymentReference == nil || *replay.PaymentReference != "cs_update_1" {
		t.Errorf("replay changed payment_reference: %v", replay.PaymentReference)
	}
}
