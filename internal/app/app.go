// Package app is the composition root: the only place that knows both the core ports
// and the concrete adapters. Everything else depends on interfaces (DESIGN.md §1.2).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
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
	Payments  ports.Payments // nil until C6
	Notify    ports.Notifier // nil until C7
	Log       *slog.Logger

	// Agent is the stateless answered-call loop; Runner owns one call attempt. There is
	// deliberately no public HTTP call endpoint in C3 (see docs/agent.md): callers invoke
	// Runner.Run directly until C5 owns authenticated scheduling.
	Agent  *agent.Agent
	Runner *runner.Runner

	// Webhooks is the provider-facing HTTP surface (Twilio webhooks/Media Streams).
	// Nil when the selected provider has none. internal/httpapi mounts it; the core
	// never sees it.
	Webhooks http.Handler
}

// New validates nothing itself (config.Load does that) but fails when a configured
// provider has no adapter yet: a clear "not implemented" beats a half-wired binary.
func New(cfg config.Config) (*App, error) {
	log := newLogger(cfg.Log)

	storage, err := buildStorage(cfg.Storage)
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
	})
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}

	return &App{
		Config:    cfg,
		Storage:   storage,
		Telephony: telephony,
		STT:       stt,
		TTS:       tts,
		Dialogue:  dialogue,
		Log:       log,
		Agent:     ag,
		Runner:    run,
		Webhooks:  webhooks,
	}, nil
}

// Close drains active calls before process shutdown. Storage and adapters need no close
// API in C3.
func (a *App) Close(ctx context.Context) error {
	if a == nil || a.Runner == nil {
		return nil
	}
	return a.Runner.Close(ctx)
}
