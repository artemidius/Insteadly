package domain

import "time"

// UserID identifies a registered account.
type UserID string

// NewUserID returns a fresh random user identifier (32 lowercase hex chars).
func NewUserID() UserID { return UserID(newID()) }

// User is a registered account. PasswordHash is the bcrypt hash and is never serialized.
type User struct {
	ID           UserID    `json:"id"`
	Email        string    `json:"email"` // normalized lowercase
	PasswordHash string    `json:"-"`     // bcrypt string; never marshal/log
	CreatedAt    time.Time `json:"created_at"`
}

// Session is one active login. TokenHash is sha256(hex) of the raw cookie token; the raw
// token is never stored, so a store leak yields no usable sessions.
type Session struct {
	TokenHash string    `json:"-"`
	UserID    UserID    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}
