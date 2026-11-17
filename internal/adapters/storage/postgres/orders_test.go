//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func sampleOrder() domain.Order {
	return domain.Order{
		Clinic: domain.Clinic{Name: "Praxis Test", Phone: "+431234567"},
		Patient: domain.Patient{
			FullName:        "Max Mustermann",
			Phone:           "+436641234567",
			BirthDate:       "1985-04-12",
			InsuranceNumber: "1234567890",
			Reason:          "Kontrolle",
			PreferredSlots: []domain.TimeRange{{
				Start: time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC),
				End:   time.Date(2027, 1, 15, 10, 0, 0, 0, time.UTC),
			}},
		},
	}
}

func createOrder(t *testing.T, store interface {
	Create(context.Context, domain.Order) (domain.Order, error)
}, o domain.Order) domain.Order {
	t.Helper()
	created, err := store.Create(context.Background(), o)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func TestCreateGetRoundTrip(t *testing.T) {
	store := newTestStore(t)
	created := createOrder(t, store, sampleOrder())
	if created.ID == "" || created.Status != domain.OrderCreated {
		t.Fatalf("created = %+v", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", created)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID || got.Status != domain.OrderCreated {
		t.Errorf("got = %+v", got)
	}
	if !reflect.DeepEqual(got.Clinic, created.Clinic) || !reflect.DeepEqual(got.Patient, created.Patient) {
		t.Errorf("JSONB round-trip lost data:\n got %+v\nwant %+v", got, created)
	}
	if got.Outcome != nil || got.Recording != nil {
		t.Errorf("nil optional fields became non-nil: %+v", got)
	}
	if len(got.AttemptHistory) != 0 {
		t.Errorf("fresh order history = %+v", got.AttemptHistory)
	}
	if delta := got.CreatedAt.Sub(created.CreatedAt); delta > time.Microsecond || delta < -time.Microsecond {
		t.Errorf("created_at drifted by %v", delta)
	}
}

func TestCreateForcesCreatedStatusAndConflict(t *testing.T) {
	store := newTestStore(t)
	o := sampleOrder()
	o.Status = domain.OrderPaid
	created := createOrder(t, store, o)
	if created.Status != domain.OrderCreated {
		t.Errorf("status = %q, want created", created.Status)
	}
	if _, err := store.Create(context.Background(), domain.Order{ID: created.ID}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate Create err = %v, want ErrConflict", err)
	}
}

func TestGetMissingAndMalformed(t *testing.T) {
	store := newTestStore(t)
	for _, id := range []domain.OrderID{domain.NewOrderID(), "../../etc/passwd"} {
		if _, err := store.Get(context.Background(), id); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("Get(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestListNewestFirst(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	first := sampleOrder()
	first.CreatedAt = base
	first.UpdatedAt = base
	second := sampleOrder()
	second.CreatedAt = base.Add(time.Hour)
	second.UpdatedAt = second.CreatedAt
	createdFirst := createOrder(t, store, first)
	createdSecond := createOrder(t, store, second)

	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].ID != createdSecond.ID || list[1].ID != createdFirst.ID {
		t.Errorf("list order = %s,%s, want %s,%s", list[0].ID, list[1].ID, createdSecond.ID, createdFirst.ID)
	}
}

func TestJSONBFidelity(t *testing.T) {
	store := newTestStore(t)
	order := createOrder(t, store, sampleOrder())

	callbackAt := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(3 * time.Minute)
	historyOutcome := domain.Outcome{
		Kind:       domain.OutcomeFailed,
		Detail:     "busy",
		FailReason: "busy",
	}
	history := []domain.AttemptRecord{{
		Number:    1,
		Outcome:   historyOutcome,
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}}
	yes, no := true, false
	recording := &domain.RecordingFlags{Recording: &yes, Transcript: &no, Audio: &yes}
	finalOutcome := domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "Termin bestätigt",
		BookedSlot: &domain.BookedSlot{DateTime: time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC), Doctor: "Dr. A", Practice: "Praxis A"},
	}
	updated, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
		o.Recording = recording
		o.AttemptHistory = history
		o.NextAttemptAt = &callbackAt
		return o.Finish(finalOutcome)
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.NextAttemptAt == nil || !updated.NextAttemptAt.Equal(callbackAt) {
		t.Errorf("returned next_attempt_at = %v", updated.NextAttemptAt)
	}

	got, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeBooked ||
		got.Outcome.BookedSlot == nil || !got.Outcome.BookedSlot.DateTime.Equal(finalOutcome.BookedSlot.DateTime) ||
		got.Outcome.BookedSlot.Doctor != "Dr. A" || got.Outcome.Detail != "Termin bestätigt" {
		t.Fatalf("outcome = %+v", got.Outcome)
	}
	if got.Recording == nil || got.Recording.Recording == nil || !*got.Recording.Recording ||
		got.Recording.Transcript == nil || *got.Recording.Transcript || got.Recording.Audio == nil || !*got.Recording.Audio {
		t.Fatalf("recording = %+v", got.Recording)
	}
	if len(got.AttemptHistory) != 1 {
		t.Fatalf("history = %+v", got.AttemptHistory)
	}
	rec := got.AttemptHistory[0]
	if rec.Number != 1 || rec.Outcome.FailReason != "busy" || !rec.StartedAt.Equal(startedAt) || !rec.EndedAt.Equal(endedAt) {
		t.Errorf("history record = %+v", rec)
	}
	if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(callbackAt) {
		t.Errorf("next_attempt_at = %v, want %v", got.NextAttemptAt, callbackAt)
	}
}

func TestUpdateRowLockSerializes(t *testing.T) {
	store := newTestStore(t)
	order := createOrder(t, store, sampleOrder())

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
				o.Attempts++
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update: %v", err)
		}
	}
	got, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Attempts != workers {
		t.Errorf("attempts = %d, want %d", got.Attempts, workers)
	}
}

func TestUpdateRollbackAndIDGuard(t *testing.T) {
	store := newTestStore(t)
	order := createOrder(t, store, sampleOrder())
	sentinel := errors.New("nope")
	if _, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
		o.Attempts = 99
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	got, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Attempts != 0 {
		t.Errorf("rollback failed: attempts = %d", got.Attempts)
	}
	if _, err := store.Update(context.Background(), order.ID, func(o *domain.Order) error {
		o.ID = domain.NewOrderID()
		return nil
	}); err == nil {
		t.Fatal("Update allowed the callback to change the ID")
	}
}

func TestMigrateIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var migrations int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrations != 1 {
		t.Errorf("schema_migrations = %d, want 1", migrations)
	}
}

func TestContextCancellation(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List err = %v, want context.Canceled", err)
	}
	order := createOrder(t, store, sampleOrder())
	if _, err := store.Get(ctx, order.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v, want context.Canceled", err)
	}
}
