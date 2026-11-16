// Package config holds the typed configuration surface and the environment loader.
//
// The config mirrors DESIGN.md §2.3: providers are swappable by name, and only the
// composition root (internal/app) resolves those names into concrete adapters. Secrets
// (Twilio, Mistral) live in this struct but must never reach logs — see Summary.
package config

import (
	"fmt"
	"slices"
	"strings"
)

// Config is the complete, validated configuration of the binary.
type Config struct {
	HTTP      HTTP
	Telephony Telephony
	STT       STT
	TTS       TTS
	Dialogue  Dialogue
	Storage   Storage
	Recording Policy
	Mistral   Mistral // shared by STT/TTS/Dialogue when their provider == "mistral"
	Log       Log
}

// HTTP configures the API listener.
type HTTP struct {
	Addr string // host:port
}

// Telephony selects the telephony adapter and holds its settings.
type Telephony struct {
	Provider string // "twilio" | "stub"
	Twilio   Twilio
}

// Twilio holds the Twilio adapter settings. AccountSID and AuthToken are secrets.
type Twilio struct {
	AccountSID  string // secret
	AuthToken   string // secret
	PhoneNumber string // caller-id number bought in Twilio console (needed C1)
	Mode        string // "voice-api" | "sip-trunk" (needed C1)
}

// STT selects the speech-to-text adapter.
type STT struct{ Provider string } // "mistral" | "stub"

// TTS selects the text-to-speech adapter.
type TTS struct{ Provider string } // "mistral" | "stub"

// Dialogue selects the dialogue (LLM) adapter.
type Dialogue struct{ Provider string } // "mistral" | "stub"

// Storage selects the persistence adapter.
type Storage struct {
	Provider string // "local"
	Local    Local
}

// Local configures the local-disk storage adapter.
type Local struct {
	Dir string // data dir, default "./data"
}

// Policy holds the global recording defaults (DESIGN.md §3.4).
type Policy struct {
	RecordingEnabled  bool
	TranscriptEnabled bool
	AudioEnabled      bool
}

// Mistral holds the shared Mistral API settings (needed C2). APIKey is a secret.
type Mistral struct {
	APIKey string // secret; required only when a mistral provider is selected (C2)
}

// Log configures structured logging.
type Log struct {
	Level  string // debug|info|warn|error
	Format string // text|json
}

var (
	telephonyProviders = []string{"twilio", "stub"}
	twilioModes        = []string{"voice-api", "sip-trunk"}
	aiProviders        = []string{"mistral", "stub"}
	storageProviders   = []string{"local"}
	logLevels          = []string{"debug", "info", "warn", "error"}
	logFormats         = []string{"text", "json"}
)

// Validate checks enum values and required fields. It does NOT check whether an
// adapter for a provider exists yet — app.New does that (so "twilio" passes here
// and fails with ErrNotImplemented in the composition root until C1).
//
// The first violation is returned, prefixed with "config: ".
func (c Config) Validate() error {
	if strings.TrimSpace(c.HTTP.Addr) == "" {
		return fmt.Errorf("config: http.addr is required")
	}
	if err := checkEnum("storage.provider", c.Storage.Provider, storageProviders); err != nil {
		return err
	}
	if strings.TrimSpace(c.Storage.Local.Dir) == "" {
		return fmt.Errorf("config: storage.local.dir is required")
	}
	if err := checkEnum("log.level", c.Log.Level, logLevels); err != nil {
		return err
	}
	if err := checkEnum("log.format", c.Log.Format, logFormats); err != nil {
		return err
	}
	if err := checkEnum("telephony.provider", c.Telephony.Provider, telephonyProviders); err != nil {
		return err
	}
	if err := checkEnum("telephony.twilio.mode", c.Telephony.Twilio.Mode, twilioModes); err != nil {
		return err
	}
	if err := checkEnum("stt.provider", c.STT.Provider, aiProviders); err != nil {
		return err
	}
	if err := checkEnum("tts.provider", c.TTS.Provider, aiProviders); err != nil {
		return err
	}
	return checkEnum("dialogue.provider", c.Dialogue.Provider, aiProviders)
}

func checkEnum(name, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("config: %s: unknown value %q (want one of %s)", name, value, strings.Join(allowed, ", "))
}

// Summary returns non-secret values for startup logging and /healthz.
// NEVER include AccountSID, AuthToken, or APIKey.
func (c Config) Summary() map[string]any {
	return map[string]any{
		"http.addr":          c.HTTP.Addr,
		"telephony.provider": c.Telephony.Provider,
		"stt.provider":       c.STT.Provider,
		"tts.provider":       c.TTS.Provider,
		"dialogue.provider":  c.Dialogue.Provider,
		"storage.provider":   c.Storage.Provider,
		"storage.local.dir":  c.Storage.Local.Dir,
		"recording.enabled":  c.Recording.RecordingEnabled,
		"transcript.enabled": c.Recording.TranscriptEnabled,
		"audio.enabled":      c.Recording.AudioEnabled,
		"log.level":          c.Log.Level,
		"log.format":         c.Log.Format,
	}
}
