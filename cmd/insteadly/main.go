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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the scheduler before serving traffic: an order placed in the same process
	// must be pickable by the loop. Start is idempotent and a no-op when disabled.
	a.Start(ctx)

	srv := httpapi.New(cfg, a, version)

	errCh := make(chan error, 1)
	go func() {
		a.Log.Info("http listening", "addr", cfg.HTTP.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		// The listener failed or was closed unexpectedly; still drain scheduler, calls,
		// the event hub, HTTP, and finally the storage pool.
		shErr := shutdown(a, srv)
		if errors.Is(err, http.ErrServerClosed) {
			return shErr
		}
		return errors.Join(err, shErr)
	case <-ctx.Done():
		a.Log.Info("shutting down")
		return shutdown(a, srv)
	}
}

// shutdown implements the C5 order (plan §8): Scheduler → Runner → event hub → HTTP →
// Postgres pool last. Active calls need the telephony HTTP/WebSocket surface while they
// cancel and hang up, so the HTTP server drains after the runner, not before it. Each
// stage gets its own bound so one slow stage cannot starve the next.
func shutdown(a *app.App, srv *http.Server) error {
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 15*time.Second)
	closeErr := a.Close(closeCtx)
	cancelClose()

	srvCtx, cancelSrv := context.WithTimeout(context.Background(), 5*time.Second)
	srvErr := srv.Shutdown(srvCtx)
	cancelSrv()

	storageErr := a.CloseStorage()
	return errors.Join(closeErr, srvErr, storageErr)
}
