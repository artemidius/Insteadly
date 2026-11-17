// Package log is the zero-side-effect notifier: it records each notification in the
// structured log and never contacts a provider. It is the default when notifications are
// explicitly enabled (notify.provider=log).
package log

import (
	"context"
	"io"
	"log/slog"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Notifier implements ports.Notifier by logging.
type Notifier struct {
	log *slog.Logger
}

var _ ports.Notifier = (*Notifier)(nil)

// New builds a log notifier. A nil logger discards output.
func New(log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Notifier{log: log}
}

// Send logs the non-secret envelope at Info. The body may carry patient detail
// (appointment reason, callback context), so only its length is logged, never its text.
func (n *Notifier) Send(ctx context.Context, notification domain.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.log.Info("notification",
		"order_id", notification.OrderID,
		"channel", notification.Channel,
		"to", notification.To,
		"subject", notification.Subject,
		"body_bytes", len(notification.Body),
	)
	return nil
}
