package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func testNotification() domain.Notification {
	return domain.Notification{
		OrderID: "0123456789abcdef0123456789abcdef",
		Channel: "email",
		To:      "patient@example.test",
		Subject: "Termin bestätigt",
		Body:    "Patient Max Mustermann, Kontrolle am 20.11.2026",
	}
}

func TestSendLogsEnvelopeAndRedactsBody(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	n := New(logger)

	if err := n.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"notification",
		"0123456789abcdef0123456789abcdef",
		"email",
		"patient@example.test",
		"Termin bestätigt",
		"body_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Max Mustermann") || strings.Contains(out, "Kontrolle") {
		t.Fatalf("log output leaked the notification body: %s", out)
	}
}

func TestSendHonorsCanceledContext(t *testing.T) {
	n := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Send(ctx, testNotification()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	if err := New(nil).Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("Send with nil logger: %v", err)
	}
}
