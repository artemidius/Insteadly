// Package ports defines the contracts the core consumes. Every implementation lives
// under internal/adapters; the core never imports an adapter (DESIGN.md §1.2, §2.2).
//
// The doc comments are the executable contract: they specify error semantics, channel
// closure rules, and backpressure behavior that every adapter must honor.
package ports

import "errors"

// Sentinel errors the core expects from adapters.
var (
	// ErrNotFound is returned when a requested record or blob does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a create would overwrite an existing record.
	ErrConflict = errors.New("conflict")
	// ErrNotImplemented marks a provider that is configured but not wired yet.
	ErrNotImplemented = errors.New("not implemented")
)
