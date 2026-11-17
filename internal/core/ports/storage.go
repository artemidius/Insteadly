package ports

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Storage persists order state and blobs (DESIGN.md §2.2). Implementations may be
// local disk (C0) or Postgres+S3/MinIO (later); the core sees one interface.
type Storage interface {
	// Orders returns the store for structured order state.
	Orders() OrderStore
	// Blobs returns the store for recordings and transcripts.
	Blobs() BlobStore
}

// OrderStore persists the order state machine.
type OrderStore interface {
	// Create persists a new order. If o.ID is empty the store assigns one
	// (returned). Returns ErrConflict if the ID already exists.
	Create(ctx context.Context, o domain.Order) (domain.Order, error)

	// Get returns ErrNotFound if the order does not exist.
	Get(ctx context.Context, id domain.OrderID) (domain.Order, error)

	// List returns all orders, newest first (by CreatedAt).
	List(ctx context.Context) ([]domain.Order, error)

	// Update atomically applies fn to the stored order and persists the result.
	// fn receives a copy; it must not touch o.ID. fn runs under the store's lock,
	// so it must be fast and must not call back into the store. Returns the
	// updated order.
	Update(ctx context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error)
}

// BlobStore persists opaque blobs, addressable by domain.BlobKey.
type BlobStore interface {
	// Put stores a blob under key. Safe to call with the same key repeatedly
	// (last write wins).
	Put(ctx context.Context, key domain.BlobKey, r io.Reader) error

	// Open returns a reader for the blob, or ErrNotFound.
	Open(ctx context.Context, key domain.BlobKey) (io.ReadCloser, error)

	// Delete removes a blob. Idempotent: deleting a missing blob is not an error.
	Delete(ctx context.Context, key domain.BlobKey) error
}

// ErrNotEligible is returned by SchedulerStore.Claim when the order is no longer
// claimable (not due, already claimed by another worker, or no longer in an eligible
// status). The scheduler treats it as a benign miss, never an error to surface.
var ErrNotEligible = errors.New("not eligible")

// SchedulerStore is the narrow surface the scheduler consumes (DESIGN §2.2). It is
// deliberately separate from OrderStore: listing and lease-claiming due orders is a
// cross-process coordination concern that the generic CRUD store does not express.
//
// A claim is a LEASE, not a status change. Claim never moves an order to "dialing";
// the runner's own scheduled→dialing Update remains the single duplicate-dial fence.
type SchedulerStore interface {
	// ListEligible returns up to limit orders that are due now and not leased by
	// another worker. Eligible means: status == paid, OR (status == scheduled AND
	// (next_attempt_at IS NULL OR next_attempt_at <= now)), AND (lease_until IS NULL
	// OR lease_until <= now). Orders are returned in a stable order (next_attempt_at,
	// then id) so repeated calls are deterministic under a fixed clock.
	ListEligible(ctx context.Context, now time.Time, limit int) ([]domain.Order, error)

	// Claim atomically leases one eligible order to owner until leaseUntil, returning
	// the leased order. It returns ErrNotEligible (never ErrNotFound) when the order
	// is no longer claimable; the caller retries another candidate. The order's
	// status is left unchanged. Claim must be safe to call from many processes.
	//
	// Due-ness is re-checked against the implementation's wall clock; the caller's
	// ListEligible snapshot decides which candidates to attempt, and the runner's
	// scheduled→dialing transition remains the final duplicate-dial fence.
	Claim(ctx context.Context, id domain.OrderID, owner string, leaseUntil time.Time) (domain.Order, error)

	// Release clears a lease held by owner, best-effort. It is idempotent and never
	// returns an error for a missing/already-released order. Release exists so a
	// worker that drops an order (e.g. Runner.Run rejects it) does not burn the full
	// lease; lease expiry remains the crash-safety net.
	Release(ctx context.Context, id domain.OrderID, owner string) error
}
