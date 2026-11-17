package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

type fakeStore struct {
	users    map[string]domain.User // keyed by normalized email
	byID     map[domain.UserID]domain.User
	sessions map[string]domain.Session
	creates  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    make(map[string]domain.User),
		byID:     make(map[domain.UserID]domain.User),
		sessions: make(map[string]domain.Session),
	}
}

func (f *fakeStore) CreateUser(_ context.Context, email, passwordHash string) (domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, ok := f.users[email]; ok {
		return domain.User{}, ports.ErrConflict
	}
	user := domain.User{
		ID:           domain.NewUserID(),
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
	}
	f.users[email] = user
	f.byID[user.ID] = user
	f.creates++
	return user, nil
}

func (f *fakeStore) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	user, ok := f.users[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return domain.User{}, ports.ErrNotFound
	}
	return user, nil
}

func (f *fakeStore) GetUser(_ context.Context, id domain.UserID) (domain.User, error) {
	user, ok := f.byID[id]
	if !ok {
		return domain.User{}, ports.ErrNotFound
	}
	return user, nil
}

func (f *fakeStore) CreateSession(_ context.Context, s domain.Session) error {
	f.sessions[s.TokenHash] = s
	return nil
}

func (f *fakeStore) GetSession(_ context.Context, tokenHash string) (domain.Session, error) {
	s, ok := f.sessions[tokenHash]
	if !ok || !time.Now().Before(s.ExpiresAt) {
		return domain.Session{}, ports.ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) DeleteSession(_ context.Context, tokenHash string) error {
	delete(f.sessions, tokenHash)
	return nil
}

type fakeHasher struct {
	hashCalls   int
	verifyCalls int
}

func (h *fakeHasher) Hash(_ context.Context, password string) (string, error) {
	h.hashCalls++
	return "hashed:" + password, nil
}

func (h *fakeHasher) Verify(_ context.Context, password, hash string) bool {
	h.verifyCalls++
	return hash == "hashed:"+password
}

func newService(t *testing.T) (*Service, *fakeStore, *fakeHasher) {
	t.Helper()
	store := newFakeStore()
	hasher := &fakeHasher{}
	return New(store, hasher, time.Hour), store, hasher
}

func TestRegisterHashesStoresAndOpensSession(t *testing.T) {
	svc, store, hasher := newService(t)
	user, token, err := svc.Register(context.Background(), "  User@Example.TEST ", "correct horse battery")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.Email != "user@example.test" {
		t.Errorf("email = %q, want normalized lowercase", user.Email)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(string(user.ID)) {
		t.Errorf("user id = %q, want 32-hex", user.ID)
	}
	if user.PasswordHash != "hashed:correct horse battery" {
		t.Errorf("stored hash = %q", user.PasswordHash)
	}
	if hasher.hashCalls != 1 {
		t.Errorf("hash calls = %d, want 1", hasher.hashCalls)
	}
	if token == "" {
		t.Fatal("token is empty")
	}
	session, ok := store.sessions[TokenHash(token)]
	if !ok {
		t.Fatal("session was not stored under sha256(token)")
	}
	if session.UserID != user.ID {
		t.Errorf("session user = %q, want %q", session.UserID, user.ID)
	}
	if ttl := time.Until(session.ExpiresAt); ttl <= 0 || ttl > time.Hour+time.Minute {
		t.Errorf("session ttl = %v, want about 1h", ttl)
	}
	if strings.Contains(token, "hashed") {
		t.Error("raw token looks like a password hash")
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	svc, _, _ := newService(t)
	if _, _, err := svc.Register(context.Background(), "dup@example.test", "password123"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, _, err := svc.Register(context.Background(), "DUP@example.test", "password456")
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ports.ErrConflict", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	cases := map[string]struct {
		email    string
		password string
		wants    []string
	}{
		"empty email":      {"", "password123", []string{"email is required"}},
		"no at":            {"not-an-email", "password123", []string{"name@example.com"}},
		"empty local part": {"@example.test", "password123", []string{"name@example.com"}},
		"empty domain":     {"user@", "password123", []string{"name@example.com"}},
		"two at":           {"a@b@c", "password123", []string{"name@example.com"}},
		"short password":   {"user@example.test", "short", []string{"at least 8"}},
		"long password":    {"user@example.test", strings.Repeat("a", 73), []string{"at most 72 bytes"}},
		"both invalid":     {"bogus", "x", []string{"name@example.com", "at least 8"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, store, hasher := newService(t)
			_, _, err := svc.Register(context.Background(), tc.email, tc.password)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(ve.Error(), want) {
					t.Errorf("problems %q do not mention %q", ve.Error(), want)
				}
			}
			if store.creates != 0 || hasher.hashCalls != 0 {
				t.Error("invalid input reached the store or hasher")
			}
		})
	}
}

func TestLogin(t *testing.T) {
	svc, _, _ := newService(t)
	user, _, err := svc.Register(context.Background(), "login@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, token, err := svc.Login(context.Background(), "LOGIN@example.test", "password123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.ID != user.ID || token == "" {
		t.Errorf("login = %+v token %q", got, token)
	}
	if _, err := svc.Authenticate(context.Background(), token); err != nil {
		t.Errorf("Authenticate(login token): %v", err)
	}

	for name, tc := range map[string]struct{ email, password string }{
		"unknown email": {"nobody@example.test", "password123"},
		"bad password":  {"login@example.test", "wrong-password"},
		"empty email":   {"", "password123"},
		"short guess":   {"login@example.test", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := svc.Login(context.Background(), tc.email, tc.password)
			if !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("err = %v, want ErrBadCredentials", err)
			}
		})
	}
}

func TestLogoutRevokesAndIsIdempotent(t *testing.T) {
	svc, _, _ := newService(t)
	_, token, err := svc.Register(context.Background(), "logout@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), token); err != nil {
		t.Fatalf("Authenticate before logout: %v", err)
	}
	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate after logout err = %v, want ErrUnauthenticated", err)
	}
	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Fatalf("empty Logout: %v", err)
	}
}

func TestAuthenticateRejectsMissingUnknownExpired(t *testing.T) {
	svc, store, _ := newService(t)
	user, _, err := svc.Register(context.Background(), "auth@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Unknown token.
	if _, err := svc.Authenticate(context.Background(), "unknown-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown token err = %v", err)
	}
	// Empty token.
	if _, err := svc.Authenticate(context.Background(), "   "); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("empty token err = %v", err)
	}
	// Expired session (present in the store, but the service checks expiry too).
	expired := "expired-token"
	store.sessions[TokenHash(expired)] = domain.Session{
		TokenHash: TokenHash(expired),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	if _, err := svc.Authenticate(context.Background(), expired); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired token err = %v", err)
	}
	// Valid token resolves the user.
	_, token, err := svc.Login(context.Background(), "auth@example.test", "password123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	got, err := svc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("user = %q, want %q", got.ID, user.ID)
	}
}

func TestTokenUniquenessAndHashFormat(t *testing.T) {
	svc, store, _ := newService(t)
	_, first, err := svc.Register(context.Background(), "a@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, second, err := svc.Register(context.Background(), "b@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if first == second {
		t.Error("two sessions got the same token")
	}
	if len(store.sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(store.sessions))
	}

	sum := sha256.Sum256([]byte(first))
	want := hex.EncodeToString(sum[:])
	if got := TokenHash(first); got != want {
		t.Errorf("TokenHash = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(TokenHash(first)) {
		t.Errorf("TokenHash is not 64 lowercase hex: %q", TokenHash(first))
	}
	if _, rawStored := store.sessions[first]; rawStored {
		t.Error("raw token is stored as a session key")
	}
}

func TestNewDefaultsTTL(t *testing.T) {
	svc := New(newFakeStore(), &fakeHasher{}, 0)
	if svc.SessionTTL() != defaultTTL {
		t.Errorf("ttl = %v, want %v", svc.SessionTTL(), defaultTTL)
	}
}
