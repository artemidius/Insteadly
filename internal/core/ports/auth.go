package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// AuthStore persists users and login sessions (C6). Implementations: storage/local (JSON
// users + in-memory sessions) and storage/postgres (users/sessions tables). Passwords are
// NOT handled here — hashing is the PasswordHasher port's job.
type AuthStore interface {
	// CreateUser stores a new user with an already-hashed password. Returns ErrConflict
	// when the email (normalized lowercase) is taken.
	CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error)

	// GetUserByEmail returns the user or ErrNotFound. Email comparison is
	// case-insensitive (the store normalizes to lowercase).
	GetUserByEmail(ctx context.Context, email string) (domain.User, error)

	// GetUser returns the user by ID or ErrNotFound.
	GetUser(ctx context.Context, id domain.UserID) (domain.User, error)

	// CreateSession stores a session keyed by tokenHash (sha256 hex of the raw token).
	CreateSession(ctx context.Context, s domain.Session) error

	// GetSession returns the session by tokenHash, or ErrNotFound when missing or expired.
	GetSession(ctx context.Context, tokenHash string) (domain.Session, error)

	// DeleteSession revokes a session. Idempotent: deleting a missing session is not an error.
	DeleteSession(ctx context.Context, tokenHash string) error
}

// PasswordHasher hashes and verifies passwords. bcrypt adapter in C6; the core auth
// service depends on this port, never on a specific hash algorithm.
type PasswordHasher interface {
	// Hash returns a salted, verifiable hash of password. Never returns the plaintext.
	Hash(ctx context.Context, password string) (string, error)

	// Verify reports whether password matches hash, in constant time where the algorithm
	// allows. A malformed hash must return false, not an error.
	Verify(ctx context.Context, password, hash string) bool
}
