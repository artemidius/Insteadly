package app

import (
	"log/slog"
	"os"

	"github.com/artemidius/insteadly/internal/core/config"
)

// newLogger builds the process logger. config.Validate already rejects unknown levels
// and formats; the switch below still falls back to safe defaults so the function is
// correct on its own.
func newLogger(c config.Log) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(c.Level)}

	var h slog.Handler
	if c.Format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
