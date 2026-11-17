// Package bcrypt implements ports.PasswordHasher with golang.org/x/crypto/bcrypt. It is
// an adapter: the core auth service never imports it.
package bcrypt

import (
	"context"
	"fmt"

	xbcrypt "golang.org/x/crypto/bcrypt"

	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	// DefaultCost is the application default (bcrypt's own DefaultCost is 10; C6 uses 12
	// for a wider safety margin at prototype login rates).
	DefaultCost = 12
	// MinCost and MaxCost are bcrypt's own bounds.
	MinCost = xbcrypt.MinCost
	MaxCost = xbcrypt.MaxCost
)

// Hasher is a ports.PasswordHasher backed by bcrypt.
type Hasher struct {
	cost int
}

var _ ports.PasswordHasher = (*Hasher)(nil)

// New builds a bcrypt hasher. cost 0 selects DefaultCost; anything outside bcrypt's
// [4,31] range is rejected so a bad config fails at composition instead of on first login.
func New(cost int) (*Hasher, error) {
	if cost == 0 {
		cost = DefaultCost
	}
	if cost < MinCost || cost > MaxCost {
		return nil, fmt.Errorf("bcrypt: cost %d is outside [%d,%d]", cost, MinCost, MaxCost)
	}
	return &Hasher{cost: cost}, nil
}

// Hash returns a salted bcrypt hash. The plaintext is never returned or stored.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hash, err := xbcrypt.GenerateFromPassword([]byte(password), h.cost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: hash password: %w", err)
	}
	return string(hash), nil
}

// Verify reports whether password matches hash. A malformed or empty hash is simply a
// mismatch (false), never an error.
func (h *Hasher) Verify(_ context.Context, password, hash string) bool {
	if hash == "" {
		return false
	}
	return xbcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
