// Package auth implements provider-neutral registration, login, logout, and cookie-session
// validation. It imports only core/{ports,domain} and the standard library: password
// hashing and session persistence are ports.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	tokenBytes       = 32
	defaultTTL       = 24 * time.Hour
	minPasswordLen   = 8
	maxPasswordBytes = 72 // bcrypt ignores input past 72 bytes; reject instead of silently truncating
	maxEmailLen      = 254
)

var (
	// ErrBadCredentials is returned for an unknown email and a wrong password alike, so
	// login never reveals whether an account exists.
	ErrBadCredentials = errors.New("bad credentials")
	// ErrUnauthenticated is returned when a session token is missing, unknown, or expired.
	ErrUnauthenticated = errors.New("unauthenticated")
)

// ValidationError reports one or more rejected registration fields. The HTTP layer maps
// it to 400 bad_request and includes every problem.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Service is the provider-neutral auth logic: register, login, logout, and cookie-session
// validation. It imports only core/{ports,domain} + stdlib (crypto/rand, crypto/sha256,
// encoding/hex, encoding/base64, errors, strings, time).
type Service struct {
	store  ports.AuthStore
	hasher ports.PasswordHasher
	ttl    time.Duration
	now    func() time.Time // injectable for tests
}

// New builds a Service. A non-positive ttl falls back to 24h; config validates positive
// values for real boots, but a hand-built test config should still work.
func New(store ports.AuthStore, hasher ports.PasswordHasher, ttl time.Duration) *Service {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Service{store: store, hasher: hasher, ttl: ttl, now: time.Now}
}

// SessionTTL exposes the effective session lifetime.
func (s *Service) SessionTTL() time.Duration { return s.ttl }

// Register validates email/password, hashes the password, stores the user, and opens a
// session. Returns the user and the raw session token (caller sets the cookie).
func (s *Service) Register(ctx context.Context, email, password string) (domain.User, string, error) {
	normalized := normalizeEmail(email)
	if problems := validateRegistration(normalized, password); len(problems) > 0 {
		return domain.User{}, "", &ValidationError{Problems: problems}
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return domain.User{}, "", fmt.Errorf("hash password: %w", err)
	}
	user, err := s.store.CreateUser(ctx, normalized, hash)
	if err != nil {
		return domain.User{}, "", err
	}
	token, err := s.openSession(ctx, user.ID)
	if err != nil {
		return domain.User{}, "", err
	}
	return user, token, nil
}

// Login verifies credentials and opens a session. Returns the user and the raw token.
// Bad email or password both return ErrBadCredentials (no existence oracle).
func (s *Service) Login(ctx context.Context, email, password string) (domain.User, string, error) {
	user, err := s.store.GetUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return domain.User{}, "", ErrBadCredentials
		}
		return domain.User{}, "", fmt.Errorf("look up user: %w", err)
	}
	if !s.hasher.Verify(ctx, password, user.PasswordHash) {
		return domain.User{}, "", ErrBadCredentials
	}
	token, err := s.openSession(ctx, user.ID)
	if err != nil {
		return domain.User{}, "", err
	}
	return user, token, nil
}

// Logout revokes the session for a raw token. Idempotent.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if strings.TrimSpace(rawToken) == "" {
		return nil
	}
	if err := s.store.DeleteSession(ctx, TokenHash(rawToken)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// Authenticate resolves a raw cookie token to a user. Returns ErrUnauthenticated when the
// token is empty, unknown, or expired.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (domain.User, error) {
	if strings.TrimSpace(rawToken) == "" {
		return domain.User{}, ErrUnauthenticated
	}
	session, err := s.store.GetSession(ctx, TokenHash(rawToken))
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return domain.User{}, ErrUnauthenticated
		}
		return domain.User{}, fmt.Errorf("get session: %w", err)
	}
	// Defense in depth: the store contract filters expiry, but auth must not accept an
	// expired session even if an implementation is lenient.
	if !s.now().Before(session.ExpiresAt) {
		return domain.User{}, ErrUnauthenticated
	}
	user, err := s.store.GetUser(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return domain.User{}, ErrUnauthenticated
		}
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// TokenHash exposes the sha256(hex) of a raw token so tests can assert the at-rest form.
func TokenHash(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// openSession creates a fresh opaque token and persists only its hash.
func (s *Service) openSession(ctx context.Context, userID domain.UserID) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	session := domain.Session{
		TokenHash: TokenHash(token),
		UserID:    userID,
		ExpiresAt: now.Add(s.ttl),
		CreatedAt: now,
	}
	if err := s.store.CreateSession(ctx, session); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// newToken returns a 256-bit random token, base64url-encoded without padding (43 chars).
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validateRegistration collects every problem so the client can fix them in one round
// trip. The email rule is deliberately loose (non-empty, one @ with both sides present,
// no whitespace); real deliverability checks are out of scope.
func validateRegistration(email, password string) []string {
	var problems []string
	switch {
	case email == "":
		problems = append(problems, "email is required")
	case len(email) > maxEmailLen:
		problems = append(problems, fmt.Sprintf("email must be at most %d characters", maxEmailLen))
	case strings.ContainsAny(email, " \t\r\n"):
		problems = append(problems, "email must not contain whitespace")
	default:
		local, domainPart, ok := strings.Cut(email, "@")
		if !ok || local == "" || domainPart == "" || strings.Contains(domainPart, "@") {
			problems = append(problems, "email must look like name@example.com")
		}
	}
	switch {
	case len(password) < minPasswordLen:
		problems = append(problems, fmt.Sprintf("password must be at least %d characters", minPasswordLen))
	case len(password) > maxPasswordBytes:
		problems = append(problems, fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes))
	}
	return problems
}
