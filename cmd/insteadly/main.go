// Command insteadly runs the Insteadly backend: one binary that wires the core to its
// adapters and serves the JSON API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/httpapi"
)

// version is set at build time: -ldflags "-X main.version=..."; dev otherwise.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("insteadly exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a, err := app.New(cfg)
	if err != nil {
		return err
	}
	// Summary never contains secrets; see config.Summary.
	a.Log.Info("starting insteadly", "config", cfg.Summary(), "version", version)

	srv := httpapi.New(cfg, a, version)

	errCh := make(chan error, 1)
	go func() {
		a.Log.Info("http listening", "addr", cfg.HTTP.Addr)
		errCh <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		a.Log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}
