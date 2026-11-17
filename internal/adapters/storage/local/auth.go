package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// The local store also implements ports.AuthStore for development: users are persisted in
// one JSON file, sessions live in memory (a restart logs everyone out, documented).
var _ ports.AuthStore = (*Store)(nil)

// localUser is the on-disk user record. domain.User excludes PasswordHash from JSON, so
// the adapter uses its own record to persist the hash without ever exposing it on the API.
type localUser struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) usersPath() string { return filepath.Join(s.dir, "auth", "users.json") }

// CreateUser persists a user with an already-hashed password, assigning an ID and
// timestamp. Duplicate (case-insensitive) emails return ports.ErrConflict.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	email = normalizeEmail(email)
	if email == "" {
		return domain.User{}, fmt.Errorf("local auth: email is required")
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()

	users, err := s.readUsersLocked()
	if err != nil {
		return domain.User{}, err
	}
	if _, taken := users[email]; taken {
		return domain.User{}, fmt.Errorf("local auth: user %s: %w", email, ports.ErrConflict)
	}
	user := domain.User{
		ID:           domain.NewUserID(),
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
	}
	users[email] = localUser{
		ID:           string(user.ID),
		Email:        user.Email,
		PasswordHash: user.PasswordHash,
		CreatedAt:    user.CreatedAt,
	}
	if err := s.writeUsersLocked(users); err != nil {
		return domain.User{}, err
	}
	return user, nil
}

// GetUserByEmail returns the user or an error wrapping ports.ErrNotFound. The comparison
// is case-insensitive.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()

	users, err := s.readUsersLocked()
	if err != nil {
		return domain.User{}, err
	}
	record, ok := users[normalizeEmail(email)]
	if !ok {
		return domain.User{}, fmt.Errorf("local auth: user %s: %w", normalizeEmail(email), ports.ErrNotFound)
	}
	return record.user(), nil
}

// GetUser returns the user by ID or an error wrapping ports.ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id domain.UserID) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()

	users, err := s.readUsersLocked()
	if err != nil {
		return domain.User{}, err
	}
	for _, record := range users {
		if record.ID == string(id) {
			return record.user(), nil
		}
	}
	return domain.User{}, fmt.Errorf("local auth: user %s: %w", id, ports.ErrNotFound)
}

// CreateSession stores a login session in memory, keyed by the token hash.
func (s *Store) CreateSession(ctx context.Context, session domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(session.TokenHash) == "" {
		return fmt.Errorf("local auth: session token hash is required")
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]domain.Session)
	}
	s.sessions[session.TokenHash] = session
	return nil
}

// GetSession returns the session by token hash, or ports.ErrNotFound when it is missing
// or expired.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	session, ok := s.sessions[tokenHash]
	if !ok || !time.Now().Before(session.ExpiresAt) {
		return domain.Session{}, fmt.Errorf("local auth: session: %w", ports.ErrNotFound)
	}
	return session, nil
}

// DeleteSession revokes a session. Deleting a missing session is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	delete(s.sessions, tokenHash)
	return nil
}

// readUsersLocked decodes the users file. A missing file is an empty store. The caller
// must hold authMu (read or write).
func (s *Store) readUsersLocked() (map[string]localUser, error) {
	path := s.usersPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]localUser), nil
		}
		return nil, fmt.Errorf("local auth: read users: %w", err)
	}
	users := make(map[string]localUser)
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("local auth: decode users: %w", err)
	}
	if users == nil {
		users = make(map[string]localUser)
	}
	return users, nil
}

// writeUsersLocked atomically replaces the users file (0600, parent 0750). The caller must
// hold authMu for writing.
func (s *Store) writeUsersLocked(users map[string]localUser) error {
	path := s.usersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("local auth: create users dir: %w", err)
	}
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("local auth: encode users: %w", err)
	}
	if err := writeAtomic(path, 0o600, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("local auth: %w", err)
	}
	return nil
}

func (r localUser) user() domain.User {
	return domain.User{
		ID:           domain.UserID(r.ID),
		Email:        r.Email,
		PasswordHash: r.PasswordHash,
		CreatedAt:    r.CreatedAt,
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
