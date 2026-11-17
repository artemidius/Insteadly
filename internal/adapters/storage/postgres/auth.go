package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Store implements ports.AuthStore on Postgres. Users and sessions are durable, so a
// login survives a process restart.
var _ ports.AuthStore = (*Store)(nil)

// CreateUser inserts a user with an already-hashed password. A duplicate email
// (case-insensitive because the stored form is normalized) returns ports.ErrConflict.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	email = normalizeEmail(email)
	if email == "" {
		return domain.User{}, fmt.Errorf("postgres: auth: email is required")
	}
	user := domain.User{
		ID:           domain.NewUserID(),
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, created_at)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (email) DO NOTHING`,
		string(user.ID), user.Email, user.PasswordHash, user.CreatedAt)
	if err != nil {
		return domain.User{}, fmt.Errorf("postgres: create user %s: %w", email, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.User{}, fmt.Errorf("postgres: user %s: %w", email, ports.ErrConflict)
	}
	return user, nil
}

// GetUserByEmail returns the user or an error wrapping ports.ErrNotFound. The comparison
// is case-insensitive.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	user, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE lower(email) = lower($1)`,
		normalizeEmail(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, fmt.Errorf("postgres: user %s: %w", email, ports.ErrNotFound)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("postgres: get user by email: %w", err)
	}
	return user, nil
}

// GetUser returns the user by ID or an error wrapping ports.ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id domain.UserID) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	user, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, fmt.Errorf("postgres: user %s: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("postgres: get user %s: %w", id, err)
	}
	return user, nil
}

// CreateSession stores a login session keyed by the token hash.
func (s *Store) CreateSession(ctx context.Context, session domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(session.TokenHash) == "" {
		return fmt.Errorf("postgres: auth: session token hash is required")
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at, created_at)
		 VALUES ($1,$2,$3,$4)`,
		session.TokenHash, string(session.UserID), session.ExpiresAt, session.CreatedAt); err != nil {
		return fmt.Errorf("postgres: create session: %w", err)
	}
	return nil
}

// GetSession returns the session by token hash, or ports.ErrNotFound when it is missing
// or expired (expiry is filtered in SQL).
func (s *Store) GetSession(ctx context.Context, tokenHash string) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	var (
		session domain.Session
		userID  string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT token_hash, user_id, expires_at, created_at
		 FROM sessions WHERE token_hash = $1 AND expires_at > now()`, tokenHash,
	).Scan(&session.TokenHash, &userID, &session.ExpiresAt, &session.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, fmt.Errorf("postgres: session: %w", ports.ErrNotFound)
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("postgres: get session: %w", err)
	}
	session.UserID = domain.UserID(userID)
	return session, nil
}

// DeleteSession revokes a session. Deleting a missing session is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	return nil
}

func scanUser(row rowScanner) (domain.User, error) {
	var (
		user   domain.User
		id     string
		email  string
		hash   string
		atTime time.Time
	)
	if err := row.Scan(&id, &email, &hash, &atTime); err != nil {
		return domain.User{}, err
	}
	user.ID = domain.UserID(id)
	user.Email = email
	user.PasswordHash = hash
	user.CreatedAt = atTime
	return user, nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
