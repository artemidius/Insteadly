package domain

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewUserIDFormat(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[UserID]struct{}, 64)
	for i := 0; i < 64; i++ {
		id := NewUserID()
		if !pattern.MatchString(string(id)) {
			t.Fatalf("id %q is not 32 lowercase hex", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate user id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestUserJSONOmitsPasswordHash(t *testing.T) {
	u := User{
		ID:           NewUserID(),
		Email:        "user@example.test",
		PasswordHash: "$2a$12$secret-bcrypt-hash",
		CreatedAt:    time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "secret-bcrypt-hash") ||
		strings.Contains(string(data), "password_hash") ||
		strings.Contains(string(data), "PasswordHash") {
		t.Fatalf("user JSON leaks the password hash: %s", data)
	}
	for _, want := range []string{string(u.ID), u.Email, "created_at"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("user JSON is missing %q: %s", want, data)
		}
	}
}

func TestSessionJSONOmitsTokenHash(t *testing.T) {
	s := Session{
		TokenHash: "sha256-hex-secret",
		UserID:    NewUserID(),
		ExpiresAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		CreatedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "sha256-hex-secret") || strings.Contains(string(data), "token_hash") {
		t.Fatalf("session JSON leaks the token hash: %s", data)
	}
	if !strings.Contains(string(data), string(s.UserID)) || !strings.Contains(string(data), "expires_at") {
		t.Errorf("session JSON is missing expected fields: %s", data)
	}
}
