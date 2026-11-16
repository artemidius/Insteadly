// Package app is the composition root: the only place that knows both the core ports
// and the concrete adapters. Everything else depends on interfaces (DESIGN.md §1.2).
package app

import (
	"fmt"
	"log/slog"

	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
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
}

// New validates nothing itself (config.Load does that) but fails when a configured
// provider has no adapter yet: a clear "not implemented" beats a half-wired binary.
func New(cfg config.Config) (*App, error) {
	log := newLogger(cfg.Log)

	storage, err := buildStorage(cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	telephony, err := buildTelephony(cfg.Telephony)
	if err != nil {
		return nil, fmt.Errorf("telephony: %w", err)
	}
	stt, err := buildSTT(cfg.STT)
	if err != nil {
		return nil, fmt.Errorf("stt: %w", err)
	}
	tts, err := buildTTS(cfg.TTS)
	if err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}
	dialogue, err := buildDialogue(cfg.Dialogue)
	if err != nil {
		return nil, fmt.Errorf("dialogue: %w", err)
	}

	return &App{
		Config:    cfg,
		Storage:   storage,
		Telephony: telephony,
		STT:       stt,
		TTS:       tts,
		Dialogue:  dialogue,
		Log:       log,
	}, nil
}
