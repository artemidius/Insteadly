// Package config holds the typed configuration surface and the environment loader.
//
// The config mirrors DESIGN.md §2.3: providers are swappable by name, and only the
// composition root (internal/app) resolves those names into concrete adapters. Secrets
// (Twilio, Mistral) live in this struct but must never reach logs — see Summary.
package config

import (
	"fmt"
	"net/url"
	"regexp"
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
	// PublicURL is the externally reachable https origin of this process (for example a
	// tunnel URL). Telephony webhooks use it to build the URLs Twilio is told to call,
	// and webhook signatures are validated against it. Empty when no public provider is
	// selected.
	PublicURL string
}

// Telephony selects the telephony adapter and holds its settings.
type Telephony struct {
	Provider string // "twilio" | "stub"
	Twilio   Twilio
}

// Twilio holds the Twilio adapter settings. AccountSID and AuthToken are secrets.
type Twilio struct {
	AccountSID         string // secret
	AuthToken          string // secret
	PhoneNumber        string // caller-id number bought in Twilio console
	Mode               string // "voice-api" | "sip-trunk"
	RingTimeoutSeconds int    // Twilio call Timeout; 0 = Twilio default, else 5..600
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

// Mistral holds the shared Mistral API settings. APIKey is a secret; the other
// fields are non-secret protocol/model settings that never cross a port boundary.
type Mistral struct {
	APIKey   string // secret; required only when a mistral provider is selected
	BaseURL  string
	STT      MistralSTT
	TTS      MistralTTS
	Dialogue MistralDialogue
}

// MistralSTT holds the Mistral speech-to-text settings.
type MistralSTT struct {
	RealtimeModel    string
	BatchModel       string
	StreamingDelayMS int
}

// MistralTTS holds the Mistral text-to-speech settings.
type MistralTTS struct {
	Model        string
	DefaultVoice string
	Mode         string // "realtime" | "offline"
}

// MistralDialogue holds the Mistral dialogue (LLM) settings.
type MistralDialogue struct {
	Model string
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
	mistralTTSModes    = []string{"realtime", "offline"}
	storageProviders   = []string{"local"}
	logLevels          = []string{"debug", "info", "warn", "error"}
	logFormats         = []string{"text", "json"}

	accountSIDPattern = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	e164Pattern       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
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
	if err := validatePublicURL(c.HTTP.PublicURL); err != nil {
		return err
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
	if c.Telephony.Twilio.RingTimeoutSeconds != 0 &&
		(c.Telephony.Twilio.RingTimeoutSeconds < 5 || c.Telephony.Twilio.RingTimeoutSeconds > 600) {
		return fmt.Errorf("config: telephony.twilio.ring_timeout_seconds: must be 0 (Twilio default) or between 5 and 600")
	}
	if c.Telephony.Provider == "twilio" {
		if err := validateTwilio(c.Telephony.Twilio, c.HTTP.PublicURL); err != nil {
			return err
		}
	}
	if err := checkEnum("stt.provider", c.STT.Provider, aiProviders); err != nil {
		return err
	}
	if err := checkEnum("tts.provider", c.TTS.Provider, aiProviders); err != nil {
		return err
	}
	if err := checkEnum("dialogue.provider", c.Dialogue.Provider, aiProviders); err != nil {
		return err
	}
	return c.validateMistral()
}

// validateMistral checks the shared Mistral settings. Model/voice/base-URL/delay/mode
// validation is unconditional (invalid explicit settings fail even when every provider is
// stub); only the API key is conditional on a mistral provider being selected.
func (c Config) validateMistral() error {
	if err := validateMistralBaseURL(c.Mistral.BaseURL); err != nil {
		return err
	}
	if err := checkNonEmptyTrim("mistral.stt.realtime_model", c.Mistral.STT.RealtimeModel); err != nil {
		return err
	}
	if err := checkNonEmptyTrim("mistral.stt.batch_model", c.Mistral.STT.BatchModel); err != nil {
		return err
	}
	d := c.Mistral.STT.StreamingDelayMS
	if !(d >= 80 && d <= 1200) && d != 2400 {
		return fmt.Errorf("config: mistral.stt.streaming_delay_ms: must be between 80 and 1200, or exactly 2400")
	}
	if err := checkNonEmptyTrim("mistral.tts.model", c.Mistral.TTS.Model); err != nil {
		return err
	}
	if err := checkNonEmptyTrim("mistral.tts.voice", c.Mistral.TTS.DefaultVoice); err != nil {
		return err
	}
	if err := checkEnum("mistral.tts.mode", c.Mistral.TTS.Mode, mistralTTSModes); err != nil {
		return err
	}
	if err := checkNonEmptyTrim("mistral.dialogue.model", c.Mistral.Dialogue.Model); err != nil {
		return err
	}
	if (c.STT.Provider == "mistral" || c.TTS.Provider == "mistral" || c.Dialogue.Provider == "mistral") &&
		strings.TrimSpace(c.Mistral.APIKey) == "" {
		return fmt.Errorf("config: mistral.api_key: required when a mistral provider is selected")
	}
	return nil
}

// validateMistralBaseURL accepts only an absolute https origin with no path, query,
// credentials, or fragment. It is never empty (defaults supply it); trailing slashes are
// trimmed by load.
func validateMistralBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: mistral.api_base_url: invalid URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: mistral.api_base_url: must be an https origin like https://api.mistral.ai")
	}
	return nil
}

// checkNonEmptyTrim rejects an empty value or one with surrounding whitespace. Model and
// voice identifiers are compared verbatim to provider names, so silently trimming would
// hide a copy/paste error.
func checkNonEmptyTrim(name, value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("config: %s: must not have surrounding whitespace", name)
	}
	if value == "" {
		return fmt.Errorf("config: %s: is required", name)
	}
	return nil
}

// validatePublicURL accepts an empty value (no public webhooks) or an absolute https
// origin with no path, query, or credentials. Trailing slashes are trimmed by load.
func validatePublicURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: http.public_url: invalid URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: http.public_url: must be an https origin like https://abc.trycloudflare.com")
	}
	return nil
}

// validateTwilio checks the fields the voice-api adapter cannot run without. It never
// includes a secret value in its errors.
func validateTwilio(t Twilio, publicURL string) error {
	if t.Mode == "voice-api" && publicURL == "" {
		return fmt.Errorf("config: http.public_url is required when telephony.provider is twilio (voice-api webhooks)")
	}
	if !accountSIDPattern.MatchString(t.AccountSID) {
		return fmt.Errorf("config: telephony.twilio.account_sid: must look like AC followed by 32 hex characters")
	}
	if strings.TrimSpace(t.AuthToken) == "" {
		return fmt.Errorf("config: telephony.twilio.auth_token: required for twilio")
	}
	if !e164Pattern.MatchString(t.PhoneNumber) {
		return fmt.Errorf("config: telephony.twilio.phone_number: must be E.164 like +15551234567")
	}
	return nil
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
		"http.addr":                             c.HTTP.Addr,
		"http.public_url":                       c.HTTP.PublicURL,
		"telephony.provider":                    c.Telephony.Provider,
		"telephony.twilio.mode":                 c.Telephony.Twilio.Mode,
		"telephony.twilio.ring_timeout_seconds": c.Telephony.Twilio.RingTimeoutSeconds,
		"stt.provider":                          c.STT.Provider,
		"tts.provider":                          c.TTS.Provider,
		"dialogue.provider":                     c.Dialogue.Provider,
		"storage.provider":                      c.Storage.Provider,
		"storage.local.dir":                     c.Storage.Local.Dir,
		"recording.enabled":                     c.Recording.RecordingEnabled,
		"transcript.enabled":                    c.Recording.TranscriptEnabled,
		"audio.enabled":                         c.Recording.AudioEnabled,
		"log.level":                             c.Log.Level,
		"log.format":                            c.Log.Format,
		"mistral.api_base_url":                  c.Mistral.BaseURL,
		"mistral.stt.realtime_model":            c.Mistral.STT.RealtimeModel,
		"mistral.stt.batch_model":               c.Mistral.STT.BatchModel,
		"mistral.stt.streaming_delay_ms":        c.Mistral.STT.StreamingDelayMS,
		"mistral.tts.model":                     c.Mistral.TTS.Model,
		"mistral.tts.voice":                     c.Mistral.TTS.DefaultVoice,
		"mistral.tts.mode":                      c.Mistral.TTS.Mode,
		"mistral.dialogue.model":                c.Mistral.Dialogue.Model,
	}
}
