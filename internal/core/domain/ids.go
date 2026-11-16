package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// OrderID identifies an order; CallID identifies one call attempt.
type OrderID string

// CallID identifies a single call attempt for an order.
type CallID string

// NewOrderID returns a fresh random order identifier (32 lowercase hex chars).
func NewOrderID() OrderID { return OrderID(newID()) }

// NewCallID returns a fresh random call identifier (32 lowercase hex chars).
func NewCallID() CallID { return CallID(newID()) }

// newID returns 16 random bytes as lowercase hex (32 chars).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
