// Package app is the composition root: the only place that knows both the core ports
// and the concrete adapters. Everything else depends on interfaces (DESIGN.md §1.2).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	coreauth "github.com/artemidius/insteadly/internal/core/auth"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
	"github.com/artemidius/insteadly/internal/core/scheduler"
)

// App is the fully wired object graph. It is the ONLY type that knows concrete
// adapters (dependency inversion, DESIGN.md §1.2).
type App struct {
	Config    config.Config
	Storage   ports.Storage
	Telephony ports.Telephony
	STT       ports.SpeechToText
	TTS       ports.TextToSpeech
	Dialogue  ports.Dialogue
	Payments  ports.Payments    // stub or stripe (C6)
	Auth      *coreauth.Service // user accounts/sessions (C6)
	Notify    ports.Notifier    // nil until C7
	Log       *slog.Logger

	// Agent is the stateless answered-call loop; Runner owns one call attempt.
	Agent  *agent.Agent
	Runner *runner.Runner

	// Scheduler drives due orders through Runner.Run and owns retries/requeues.
	// App.Start starts it; App.Close stops it before the runner.
	Scheduler *scheduler.Scheduler

	// Events is the in-process order-status hub. The runner/scheduler publish; the SSE
	// endpoint subscribes. App.Close closes it after the runner and scheduler stop.
	Events *events.Hub

	// Webhooks is the provider-facing HTTP surface (Twilio webhooks/Media Streams).
	// Nil when the selected provider has none. internal/httpapi mounts it; the core
	// never sees it.
	Webhooks http.Handler

	// storageCloser releases the storage handle (the Postgres pool); it is closed last,
	// after the scheduler, runner, and HTTP server are done.
	storageCloser io.Closer
}

// New validates nothing itself (config.Load does that) but fails when a configured
// provider has no adapter yet: a clear "not implemented" beats a half-wired binary.
func New(cfg config.Config) (*App, error) {
	log := newLogger(cfg.Log)
	hub := events.NewHub(events.DefaultBuffer)

	storage, schedulerStore, authStore, storageCloser, err := buildStorage(context.Background(), cfg.Storage, log)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	telephony, webhooks, err := buildTelephony(cfg.Telephony, cfg.HTTP.PublicURL, log)
	if err != nil {
		return nil, fmt.Errorf("telephony: %w", err)
	}
	stt, err := buildSTT(cfg.STT, cfg.Mistral, log)
	if err != nil {
		return nil, fmt.Errorf("stt: %w", err)
	}
	tts, err := buildTTS(cfg.TTS, cfg.Mistral, log)
	if err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}
	dialogue, err := buildDialogue(cfg.Dialogue, cfg.Mistral, log)
	if err != nil {
		return nil, fmt.Errorf("dialogue: %w", err)
	}

	hasher, err := buildPasswordHasher(cfg.Auth, log)
	if err != nil {
		return nil, fmt.Errorf("auth hasher: %w", err)
	}
	payments, err := buildPayments(cfg.Payments, cfg.HTTP.PublicURL, log)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	authService := coreauth.New(authStore, hasher, cfg.Auth.SessionTTL)

	ag, err := agent.New(agent.Config{
		STT:      stt,
		TTS:      tts,
		Dialogue: dialogue,
		Orders:   storage.Orders(),
		Logger:   log,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	run, err := runner.New(runner.Config{
		Telephony: telephony,
		Orders:    storage.Orders(),
		Agent:     ag,
		Logger:    log,
		Events:    hub,
	})
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}

	windowsJSON := cfg.Scheduler.WindowsJSON
	if strings.TrimSpace(windowsJSON) == "" {
		// config.Load supplies the default; a hand-built Config (tests, embedders) should
		// still get a working scheduler rather than an unparsable empty window set.
		windowsJSON = config.DefaultSchedulerWindows
	}
	windows, err := scheduler.ParseWindows(windowsJSON)
	if err != nil {
		return nil, err
	}
	sched, err := scheduler.New(scheduler.Config{
		Store:           schedulerStore,
		Orders:          storage.Orders(),
		Runner:          run,
		Events:          hub,
		Windows:         windows,
		Tick:            cfg.Scheduler.Tick,
		MaxAttempts:     cfg.Scheduler.MaxAttempts,
		MaxConcurrent:   cfg.Scheduler.MaxConcurrent,
		Lease:           cfg.Scheduler.Lease,
		BusyBackoff:     cfg.Scheduler.BusyBackoff,
		NoAnswerBackoff: cfg.Scheduler.NoAnswerBackoff,
		ErrorBackoff:    cfg.Scheduler.ErrorBackoff,
		Logger:          log,
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: %w", err)
	}

	return &App{
		Config:        cfg,
		Storage:       storage,
		Telephony:     telephony,
		STT:           stt,
		TTS:           tts,
		Dialogue:      dialogue,
		Payments:      payments,
		Auth:          authService,
		Log:           log,
		Agent:         ag,
		Runner:        run,
		Scheduler:     sched,
		Events:        hub,
		Webhooks:      webhooks,
		storageCloser: storageCloser,
	}, nil
}

// Start begins the scheduler when it is enabled. It is idempotent.
func (a *App) Start(ctx context.Context) {
	if a == nil || a.Scheduler == nil || !a.Config.Scheduler.Enabled {
		return
	}
	a.Scheduler.Start(ctx)
}

// Close drains the scheduler, then active calls, then the event hub. The HTTP server
// must be shut down after this and the storage pool last (CloseStorage), so a persisting
// runner or scheduler never touches a closed pool. It is idempotent.
func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.Scheduler != nil {
		if err := a.Scheduler.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("scheduler: %w", err))
		}
	}
	if a.Runner != nil {
		if err := a.Runner.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("runner: %w", err))
		}
	}
	if a.Events != nil {
		if err := a.Events.Close(); err != nil {
			errs = append(errs, fmt.Errorf("events: %w", err))
		}
	}
	return errors.Join(errs...)
}

// CloseStorage releases the storage handle (the Postgres pool). It must be called last,
// after Close and HTTP shutdown. It is idempotent.
func (a *App) CloseStorage() error {
	if a == nil || a.storageCloser == nil {
		return nil
	}
	closer := a.storageCloser
	a.storageCloser = nil
	return closer.Close()
}

// storageOpenTimeout bounds pool creation and migration during startup.
const storageOpenTimeout = 30 * time.Second
