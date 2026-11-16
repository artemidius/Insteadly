package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Notifier sends confirmations and outcomes (DESIGN.md §2.2). No adapter in C0-C6;
// first adapter lands in C7.
type Notifier interface {
	// Send delivers one notification. Errors are retryable by the caller.
	Send(ctx context.Context, n domain.Notification) error
}
