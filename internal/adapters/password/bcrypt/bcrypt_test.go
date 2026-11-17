package bcrypt

import (
	"context"
	"strings"
	"testing"
)

func TestHashVerifyRoundtrip(t *testing.T) {
	hasher, err := New(4) // cheapest cost: these tests exercise the algorithm, not latency
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash, err := hasher.Hash(context.Background(), "correct horse battery")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if hash == "correct horse battery" || strings.Contains(hash, "correct horse") {
		t.Fatalf("hash contains the plaintext: %q", hash)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("hash %q does not look like bcrypt", hash)
	}
	if !hasher.Verify(context.Background(), "correct horse battery", hash) {
		t.Error("Verify rejected the correct password")
	}
}

func TestHashIsSalted(t *testing.T) {
	hasher, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := hasher.Hash(context.Background(), "same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := hasher.Hash(context.Background(), "same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical (no salt)")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	hasher, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash, err := hasher.Hash(context.Background(), "right-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if hasher.Verify(context.Background(), "wrong-password", hash) {
		t.Error("Verify accepted the wrong password")
	}
}

func TestVerifyMalformedHashIsFalseNotError(t *testing.T) {
	hasher, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, hash := range []string{"", "not-a-bcrypt-hash", "$2a$12$too-short"} {
		if hasher.Verify(context.Background(), "password", hash) {
			t.Errorf("Verify(%q) = true, want false", hash)
		}
	}
}

func TestNewCostValidation(t *testing.T) {
	for _, cost := range []int{1, 3, MinCost - 1, MaxCost + 1, 32, 99} {
		if _, err := New(cost); err == nil {
			t.Errorf("New(%d) succeeded, want rejection", cost)
		}
	}
	for _, cost := range []int{0, MinCost, DefaultCost, MaxCost} {
		if _, err := New(cost); err != nil {
			t.Errorf("New(%d): %v", cost, err)
		}
	}
}

func TestNewZeroUsesDefaultCost(t *testing.T) {
	hasher, err := New(0)
	if err != nil {
		t.Fatalf("New(0): %v", err)
	}
	if hasher.cost != DefaultCost {
		t.Errorf("cost = %d, want %d", hasher.cost, DefaultCost)
	}
}

func TestHashTooLongPassword(t *testing.T) {
	hasher, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	long := strings.Repeat("a", 73)
	if _, err := hasher.Hash(context.Background(), long); err == nil {
		t.Error("Hash accepted a password longer than 72 bytes")
	}
}

func TestHashHonorsContext(t *testing.T) {
	hasher, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hasher.Hash(ctx, "password123"); err == nil {
		t.Error("Hash ignored a canceled context")
	}
}
