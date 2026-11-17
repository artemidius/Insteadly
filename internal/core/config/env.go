package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// envPrefix marks every variable Insteadly owns. Anything else in the environment
// (or in .env) is ignored, so unrelated tooling settings never interfere.
const envPrefix = "INSTEADLY_"

// dotEnvPath is read relative to the process working directory. Missing is fine.
const dotEnvPath = ".env"

// setter applies one environment variable to the config.
type setter struct {
	apply func(*Config, string) error
	// secret only documents which values may never be logged. Values are never
	// logged or echoed in errors regardless of this flag.
	secret bool
}

// registry is the single source of truth for the env var names: it drives the
// loader, the strict unknown-variable check, and (indirectly) the docs.
var registry = map[string]setter{
	"INSTEADLY_HTTP_ADDR":           {apply: strSetter(func(c *Config, v string) { c.HTTP.Addr = v })},
	"INSTEADLY_HTTP_PUBLIC_URL":     {apply: strSetter(func(c *Config, v string) { c.HTTP.PublicURL = v })},
	"INSTEADLY_LOG_LEVEL":           {apply: strSetter(func(c *Config, v string) { c.Log.Level = v })},
	"INSTEADLY_LOG_FORMAT":          {apply: strSetter(func(c *Config, v string) { c.Log.Format = v })},
	"INSTEADLY_TELEPHONY_PROVIDER":  {apply: strSetter(func(c *Config, v string) { c.Telephony.Provider = v })},
	"INSTEADLY_TWILIO_ACCOUNT_SID":  {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.AccountSID = v }), secret: true},
	"INSTEADLY_TWILIO_AUTH_TOKEN":   {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.AuthToken = v }), secret: true},
	"INSTEADLY_TWILIO_PHONE_NUMBER": {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.PhoneNumber = v })},
	"INSTEADLY_TWILIO_MODE":         {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.Mode = v })},
	"INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS": {
		apply: intSetter(func(c *Config, v int) { c.Telephony.Twilio.RingTimeoutSeconds = v }),
	},
	"INSTEADLY_STT_PROVIDER":       {apply: strSetter(func(c *Config, v string) { c.STT.Provider = v })},
	"INSTEADLY_TTS_PROVIDER":       {apply: strSetter(func(c *Config, v string) { c.TTS.Provider = v })},
	"INSTEADLY_DIALOGUE_PROVIDER":  {apply: strSetter(func(c *Config, v string) { c.Dialogue.Provider = v })},
	"INSTEADLY_STORAGE_PROVIDER":   {apply: strSetter(func(c *Config, v string) { c.Storage.Provider = v })},
	"INSTEADLY_STORAGE_LOCAL_DIR":  {apply: strSetter(func(c *Config, v string) { c.Storage.Local.Dir = v })},
	"INSTEADLY_RECORDING_ENABLED":  {apply: boolSetter(func(c *Config, v bool) { c.Recording.RecordingEnabled = v })},
	"INSTEADLY_TRANSCRIPT_ENABLED": {apply: boolSetter(func(c *Config, v bool) { c.Recording.TranscriptEnabled = v })},
	"INSTEADLY_AUDIO_ENABLED":      {apply: boolSetter(func(c *Config, v bool) { c.Recording.AudioEnabled = v })},
	"INSTEADLY_MISTRAL_API_KEY":    {apply: strSetter(func(c *Config, v string) { c.Mistral.APIKey = v }), secret: true},
	"INSTEADLY_MISTRAL_API_BASE_URL": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.BaseURL = v }),
	},
	"INSTEADLY_MISTRAL_STT_REALTIME_MODEL": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.STT.RealtimeModel = v }),
	},
	"INSTEADLY_MISTRAL_STT_BATCH_MODEL": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.STT.BatchModel = v }),
	},
	"INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS": {
		apply: intSetter(func(c *Config, v int) { c.Mistral.STT.StreamingDelayMS = v }),
	},
	"INSTEADLY_MISTRAL_TTS_MODEL": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.TTS.Model = v }),
	},
	"INSTEADLY_MISTRAL_TTS_VOICE": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.TTS.DefaultVoice = v }),
	},
	"INSTEADLY_MISTRAL_TTS_MODE": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.TTS.Mode = v }),
	},
	"INSTEADLY_MISTRAL_DIALOGUE_MODEL": {
		apply: strSetter(func(c *Config, v string) { c.Mistral.Dialogue.Model = v }),
	},
}

func strSetter(fn func(*Config, string)) func(*Config, string) error {
	return func(c *Config, v string) error { fn(c, v); return nil }
}

func boolSetter(fn func(*Config, bool)) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("expected true/false, got %q", v)
		}
		fn(c, b)
		return nil
	}
}

func intSetter(fn func(*Config, int)) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("expected an integer, got %q", v)
		}
		fn(c, n)
		return nil
	}
}

func defaults() Config {
	return Config{
		HTTP:      HTTP{Addr: "127.0.0.1:8080"},
		Telephony: Telephony{Provider: "stub", Twilio: Twilio{Mode: "voice-api"}},
		STT:       STT{Provider: "stub"},
		TTS:       TTS{Provider: "stub"},
		Dialogue:  Dialogue{Provider: "stub"},
		Storage:   Storage{Provider: "local", Local: Local{Dir: "./data"}},
		Mistral: Mistral{
			BaseURL: "https://api.mistral.ai",
			STT: MistralSTT{
				RealtimeModel:    "voxtral-mini-transcribe-realtime-3",
				BatchModel:       "voxtral-mini-transcribe-3",
				StreamingDelayMS: 480,
			},
			TTS: MistralTTS{
				Model:        "voxtral-mini-tts-3-0",
				DefaultVoice: "voice_447",
				Mode:         "realtime",
			},
			Dialogue: MistralDialogue{Model: "mistral-small-2603"},
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// Load applies defaults, then ./.env (if present), then the process environment,
// then validates. Strict about unknown INSTEADLY_* names.
func Load() (Config, error) {
	return load(dotEnvPath, os.Environ())
}

// load is the testable core of Load. An empty dotEnvPath means "no .env file".
func load(path string, environ []string) (Config, error) {
	c := defaults()
	if path != "" {
		if err := applyDotEnv(&c, path); err != nil {
			return c, err
		}
	}
	if err := applyEnviron(&c, environ); err != nil {
		return c, err
	}
	// A trailing slash is a common copy/paste artifact from tunnel tools and would turn
	// webhook URLs into "https://host//webhooks/...". Normalize once, before validation.
	c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
	c.Mistral.BaseURL = strings.TrimRight(strings.TrimSpace(c.Mistral.BaseURL), "/")
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// applyDotEnv parses path and applies INSTEADLY_* assignments. The format is one
// KEY=VALUE per line; blank lines and # comments are skipped. Quotes and escapes are
// deliberately not supported (tokens are plain alphanumerics) — a quoted value keeps
// its quotes, so loud breakage beats a silent half-parse.
func applyDotEnv(c *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			// Never include the line text: it may hold a secret.
			return fmt.Errorf("config: %s: line %d: expected KEY=VALUE", path, lineNo)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(name, envPrefix) {
			continue // unrelated variables (e.g. TWILIO_* without prefix) are ignored
		}
		if err := applyOne(c, name, value); err != nil {
			return fmt.Errorf("config: %s: line %d: %w", path, lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	return nil
}

// applyEnviron applies INSTEADLY_* variables from environ last, so the real process
// environment always wins over .env.
func applyEnviron(c *Config, environ []string) error {
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		if err := applyOne(c, name, value); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	return nil
}

// applyOne routes one name/value pair through the registry. Strict mode: an unknown
// INSTEADLY_* name is a hard error, because a typo silently changing policy (e.g. a
// recording toggle) is worse than a crash.
func applyOne(c *Config, name, value string) error {
	s, ok := registry[name]
	if !ok {
		return fmt.Errorf("unknown variable %q (not an Insteadly setting)", name)
	}
	if err := s.apply(c, value); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
