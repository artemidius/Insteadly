// Package webhook is the single-POST notifier: it forwards one domain.Notification as
// JSON to a configured URL. There is no signature scheme in v1 (a webhook signing secret
// is a later seam); the URL itself is a secret and is never logged.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// defaultTimeout bounds one delivery. The notify worker treats a failure as best-effort;
// a hanging endpoint must not pin the worker forever.
const defaultTimeout = 10 * time.Second

// Notifier implements ports.Notifier with one HTTP POST per notification.
type Notifier struct {
	url     string
	channel string
	client  *http.Client
	log     *slog.Logger
}

var _ ports.Notifier = (*Notifier)(nil)

// New builds a webhook notifier with the default bounded timeout.
func New(rawURL, channel string, log *slog.Logger) (*Notifier, error) {
	return NewWithClient(rawURL, channel, &http.Client{Timeout: defaultTimeout}, log)
}

// NewWithClient is New with an injected HTTP client (tests use a short timeout).
func NewWithClient(rawURL, channel string, client *http.Client, log *slog.Logger) (*Notifier, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("notify webhook: url is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("notify webhook: url must be an absolute http(s) URL")
	}
	if channel != "email" && channel != "sms" {
		return nil, fmt.Errorf("notify webhook: unsupported channel %q", channel)
	}
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Notifier{url: rawURL, channel: channel, client: client, log: log}, nil
}

// payload is the fixed, non-secret wire shape. It mirrors domain.Notification without the
// internal JSON tags (which are the API's concern, not this adapter's).
type payload struct {
	OrderID string `json:"order_id"`
	Channel string `json:"channel"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Send posts one notification. A non-2xx response is a retryable error for the caller;
// the response body is never retained or logged.
func (n *Notifier) Send(ctx context.Context, notification domain.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	channel := notification.Channel
	if channel == "" {
		channel = n.channel
	}
	raw, err := json.Marshal(payload{
		OrderID: string(notification.OrderID),
		Channel: channel,
		To:      notification.To,
		Subject: notification.Subject,
		Body:    notification.Body,
	})
	if err != nil {
		return fmt.Errorf("notify webhook: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("notify webhook: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		// *url.Error renders the full URL, which is a secret. Keep only the underlying
		// transport/context error (e.g. "connection refused", "context deadline exceeded").
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			return fmt.Errorf("notify webhook: send: %w", uerr.Err)
		}
		return errors.New("notify webhook: send failed")
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}
