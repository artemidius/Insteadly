package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestAuthCreateGetByEmailAndID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	user, err := s.CreateUser(ctx, "  User@Example.TEST ", "$2a$12$hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != "user@example.test" {
		t.Errorf("email = %q, want normalized lowercase", user.Email)
	}
	if !validOrderID(domain.OrderID(user.ID)) {
		t.Errorf("user id %q is not 32-hex", user.ID)
	}
	if user.PasswordHash != "$2a$12$hashed" || user.CreatedAt.IsZero() {
		t.Errorf("user = %+v", user)
	}

	byEmail, err := s.GetUserByEmail(ctx, "USER@example.test")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if byEmail.ID != user.ID || byEmail.PasswordHash != user.PasswordHash {
		t.Errorf("by email = %+v, want %+v", byEmail, user)
	}

	byID, err := s.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if byID.Email != user.Email {
		t.Errorf("by id = %+v", byID)
	}

	if _, err := s.GetUserByEmail(ctx, "nobody@example.test"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing email err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetUser(ctx, domain.NewUserID()); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing id err = %v, want ErrNotFound", err)
	}
}

func TestAuthDuplicateEmailConflict(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "dup@example.test", "hash-1"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err := s.CreateUser(ctx, "DUP@example.test", "hash-2")
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAuthUsersFilePersistsHashAt0600(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateUser(context.Background(), "file@example.test", "hashed-secret"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	path := filepath.Join(s.dir, "auth", "users.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat users file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("users file perm = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read users file: %v", err)
	}
	var users map[string]localUser
	if err := json.Unmarshal(data, &users); err != nil {
		t.Fatalf("decode users file: %v", err)
	}
	if got := users["file@example.test"].PasswordHash; got != "hashed-secret" {
		t.Errorf("persisted hash = %q", got)
	}
}

func TestAuthSessionLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "session@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	session := domain.Session{
		TokenHash: strings.Repeat("a", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession(ctx, session.TokenHash)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != user.ID || !got.ExpiresAt.Equal(session.ExpiresAt) {
		t.Errorf("session = %+v, want %+v", got, session)
	}
	if err := s.DeleteSession(ctx, session.TokenHash); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, session.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, "missing"); err != nil {
		t.Errorf("deleting a missing session: %v", err)
	}
}

func TestAuthExpiredSessionNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	user, err := s.CreateUser(ctx, "expired@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	expired := domain.Session{
		TokenHash: strings.Repeat("b", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(-time.Minute),
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	if err := s.CreateSession(ctx, expired); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.GetSession(ctx, expired.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("expired session err = %v, want ErrNotFound", err)
	}
}

func TestAuthReopenKeepsUsersNotSessions(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	user, err := first.CreateUser(ctx, "reopen@example.test", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	session := domain.Session{
		TokenHash: strings.Repeat("c", 64),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}
	if err := first.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	second, err := New(dir)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	if _, err := second.GetUserByEmail(ctx, "reopen@example.test"); err != nil {
		t.Errorf("users did not survive reopen: %v", err)
	}
	if _, err := second.GetSession(ctx, session.TokenHash); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("sessions should not survive reopen, err = %v", err)
	}
}
