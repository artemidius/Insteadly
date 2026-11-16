package ports

import (
	"context"
	"io"

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
