// Package config holds the typed configuration surface and the environment loader.
//
// The config mirrors DESIGN.md §2.3: providers are swappable by name, and only the
// composition root (internal/app) resolves those names into concrete adapters. Secrets
// (Twilio, Mistral) live in this struct but must never reach logs — see Summary.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the complete, validated configuration of the binary.
type Config struct {
	HTTP      HTTP
	Telephony Telephony
	STT       STT
	TTS       TTS
	Dialogue  Dialogue
	Storage   Storage
	Scheduler Scheduler
	API       API
	Recording Policy
	Mistral   Mistral // shared by STT/TTS/Dialogue when their provider == "mistral"
	OpenAI    OpenAI  // Dialogue alternative provider (C7)
	Auth      Auth
	Payments  Payments
	Notify    Notify
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
	Provider string // "stub" | "twilio" | "telnyx"
	Twilio   Twilio
	Telnyx   Telnyx
}

// Telnyx holds the Telnyx Call Control adapter settings. APIKey is a secret.
type Telnyx struct {
	APIKey             string // secret
	ConnectionID       string // Call Control connection/app id (not a credential)
	FromNumber         string // caller-id number, E.164
	RingTimeoutSeconds int    // 0 = Telnyx default, else 5..600
	// WebhookPublicKey is the account's Ed25519 public key (base64 raw key or PEM) used
	// to verify Telnyx's webhook and media-stream signatures. Optional: when unset,
	// signed requests are rejected (fail closed) and only the unguessable per-call path
	// guards the callback. It is a public key, not a secret.
	WebhookPublicKey string
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
	Provider string // "local" | "postgres"
	Local    Local
	Postgres Postgres
}

// Local configures the local-disk storage adapter. In postgres mode it still hosts
// blobs (C5 does not build a Postgres blob store).
type Local struct {
	Dir string // data dir, default "./data"
}

// Postgres configures the Postgres order/scheduler store. DSN is a secret; config only
// trims and checks non-empty, while the adapter parses it (keeping config dependency-free).
type Postgres struct {
	DSN string // secret; full libpq/URL connstring
}

// DefaultSchedulerWindows is the no-env opening window: Mon–Fri 08:00–18:00 in the
// scheduler's configured location (Europe/Vienna). It is config, not hardcoded policy.
const DefaultSchedulerWindows = `[{"dow":"mon","start":"08:00","end":"18:00"},` +
	`{"dow":"tue","start":"08:00","end":"18:00"},` +
	`{"dow":"wed","start":"08:00","end":"18:00"},` +
	`{"dow":"thu","start":"08:00","end":"18:00"},` +
	`{"dow":"fri","start":"08:00","end":"18:00"}]`

// Scheduler configures the in-process orchestration loop.
type Scheduler struct {
	Enabled         bool
	Tick            time.Duration
	MaxAttempts     int
	MaxConcurrent   int
	Lease           time.Duration
	BusyBackoff     time.Duration
	NoAnswerBackoff time.Duration
	ErrorBackoff    time.Duration
	WindowsJSON     string // JSON array: [{"dow":"mon","start":"08:00","end":"18:00"}, …]
}

// API configures the prototype place-call surface. Key is a secret; an empty key
// disables auth (development only, never production security).
type API struct {
	Key            string // secret
	IdempotencyTTL time.Duration
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

// OpenAI holds the OpenAI ChatGPT settings used by the alternative Dialogue adapter.
// APIKey is a secret; the base URL and model are non-secret protocol settings.
type OpenAI struct {
	APIKey        string // secret
	BaseURL       string
	DialogueModel string
}

// Notify selects the notification adapter and holds its settings. Provider "off" is
// the default: no notifier is built and no worker starts.
type Notify struct {
	Provider string // "off" | "log" | "webhook"
	Webhook  NotifyWebhook
}

// NotifyWebhook configures the HTTP webhook notifier. URL is a secret (it may carry a
// bearer-like query token) and is always redacted from Summary.
type NotifyWebhook struct {
	URL     string // secret
	Channel string // "email" | "sms"
}

// Auth configures user-facing cookie authentication (C6). Mode "off" (the default)
// preserves the C5 unauthenticated API; "cookie" enables registration/login and scopes
// order routes to the session user.
type Auth struct {
	Mode       string        // "off" | "cookie"
	SessionTTL time.Duration // login lifetime, also the cookie Max-Age
	CookieName string        // session cookie name
	BcryptCost int           // password hash cost, 4..31 (default 12)
}

// Payments selects the payment adapter.
type Payments struct {
	Provider string // "stub" | "stripe"
	Stripe   Stripe
}

// Stripe holds the Stripe adapter settings. SecretKey and WebhookSecret are secrets and
// are never logged; config only trims and non-empty-checks them (the adapter parses them).
type Stripe struct {
	SecretKey     string // secret
	WebhookSecret string // secret
	PriceID       string // one Stripe Price id for the prototype
}

// Log configures structured logging.
type Log struct {
	Level  string // debug|info|warn|error
	Format string // text|json
}

var (
	telephonyProviders = []string{"twilio", "stub", "telnyx"}
	twilioModes        = []string{"voice-api", "sip-trunk"}
	aiProviders        = []string{"mistral", "stub"}
	// dialogueProviders is deliberately wider than aiProviders: C7 proves the swap
	// promise with one alternative AI provider (OpenAI) on the Dialogue port only.
	dialogueProviders = []string{"mistral", "stub", "openai"}
	mistralTTSModes   = []string{"realtime", "offline"}
	storageProviders  = []string{"local", "postgres"}
	authModes         = []string{"off", "cookie"}
	paymentProviders  = []string{"stub", "stripe"}
	notifyProviders   = []string{"off", "log", "webhook"}
	notifyChannels    = []string{"email", "sms"}
	logLevels         = []string{"debug", "info", "warn", "error"}
	logFormats        = []string{"text", "json"}

	accountSIDPattern = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	e164Pattern       = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	cookieNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
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
	if c.Storage.Provider == "postgres" && strings.TrimSpace(c.Storage.Postgres.DSN) == "" {
		return fmt.Errorf("config: postgres.dsn: required when storage.provider is postgres")
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
	if c.Telephony.Telnyx.RingTimeoutSeconds != 0 &&
		(c.Telephony.Telnyx.RingTimeoutSeconds < 5 || c.Telephony.Telnyx.RingTimeoutSeconds > 600) {
		return fmt.Errorf("config: telephony.telnyx.ring_timeout_seconds: must be 0 (Telnyx default) or between 5 and 600")
	}
	if c.Telephony.Provider == "telnyx" {
		if err := validateTelnyx(c.Telephony.Telnyx, c.HTTP.PublicURL); err != nil {
			return err
		}
	}
	if err := checkEnum("stt.provider", c.STT.Provider, aiProviders); err != nil {
		return err
	}
	if err := checkEnum("tts.provider", c.TTS.Provider, aiProviders); err != nil {
		return err
	}
	if err := checkEnum("dialogue.provider", c.Dialogue.Provider, dialogueProviders); err != nil {
		return err
	}
	if err := c.validateOpenAI(); err != nil {
		return err
	}
	if err := c.validateScheduler(); err != nil {
		return err
	}
	if c.API.IdempotencyTTL <= 0 {
		return fmt.Errorf("config: api.idempotency_ttl: must be positive")
	}
	if err := c.validateAuth(); err != nil {
		return err
	}
	if err := c.validatePayments(); err != nil {
		return err
	}
	if err := c.validateNotify(); err != nil {
		return err
	}
	return c.validateMistral()
}

// validateTelnyx checks the fields the Call Control adapter cannot run without. It never
// includes a secret value in its errors.
func validateTelnyx(t Telnyx, publicURL string) error {
	if publicURL == "" {
		return fmt.Errorf("config: http.public_url is required when telephony.provider is telnyx (call-control webhooks)")
	}
	if strings.TrimSpace(t.APIKey) == "" {
		return fmt.Errorf("config: telnyx.api_key: required when telephony.provider is telnyx")
	}
	if strings.TrimSpace(t.ConnectionID) == "" {
		return fmt.Errorf("config: telnyx.connection_id: required when telephony.provider is telnyx")
	}
	if !e164Pattern.MatchString(t.FromNumber) {
		return fmt.Errorf("config: telephony.telnyx.from_number: must be E.164 like +15551234567")
	}
	return nil
}

// validateOpenAI checks the alternative Dialogue provider selection. The base URL and
// model are validated unconditionally (an explicit bad setting fails even while the
// provider is stub); only the API key is conditional on the provider being selected.
func (c Config) validateOpenAI() error {
	if err := validateOpenAIBaseURL(c.OpenAI.BaseURL); err != nil {
		return err
	}
	if err := checkNonEmptyTrim("openai.dialogue_model", c.OpenAI.DialogueModel); err != nil {
		return err
	}
	if c.Dialogue.Provider == "openai" && strings.TrimSpace(c.OpenAI.APIKey) == "" {
		return fmt.Errorf("config: openai.api_key: required when dialogue.provider is openai")
	}
	return nil
}

// validateOpenAIBaseURL accepts only an absolute https URL with no query, credentials,
// or fragment. Unlike the Mistral origin, a path is allowed because the OpenAI API base
// itself ends in /v1 (`POST {base}/chat/completions`); loopback http is permitted by the
// adapter for tests, never here, so a real key never travels over plaintext.
func validateOpenAIBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: openai.base_url: invalid URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("config: openai.base_url: must be an https URL like https://api.openai.com/v1")
	}
	return nil
}

// validateNotify checks the notification adapter selection. The channel is validated
// unconditionally; the webhook URL is required only for the webhook adapter.
func (c Config) validateNotify() error {
	if err := checkEnum("notify.provider", c.Notify.Provider, notifyProviders); err != nil {
		return err
	}
	if err := checkEnum("notify.webhook.channel", c.Notify.Webhook.Channel, notifyChannels); err != nil {
		return err
	}
	if c.Notify.Provider == "webhook" && strings.TrimSpace(c.Notify.Webhook.URL) == "" {
		return fmt.Errorf("config: notify.webhook.url: required when notify.provider is webhook")
	}
	return nil
}

// validateAuth checks the cookie-auth surface. It rejects a TTL or bcrypt cost that would
// create unusable sessions or hashes.
func (c Config) validateAuth() error {
	if err := checkEnum("auth.mode", c.Auth.Mode, authModes); err != nil {
		return err
	}
	if c.Auth.SessionTTL <= 0 {
		return fmt.Errorf("config: auth.session_ttl: must be positive")
	}
	if !cookieNamePattern.MatchString(c.Auth.CookieName) {
		return fmt.Errorf("config: auth.cookie_name: must be 1-64 letters, digits, underscores, or hyphens")
	}
	if c.Auth.BcryptCost < 4 || c.Auth.BcryptCost > 31 {
		return fmt.Errorf("config: auth.bcrypt_cost: must be between 4 and 31")
	}
	return nil
}

// validatePayments checks the payments adapter selection. Stripe mode requires all three
// settings and a public origin, because Checkout needs absolute success/cancel URLs.
func (c Config) validatePayments() error {
	if err := checkEnum("payments.provider", c.Payments.Provider, paymentProviders); err != nil {
		return err
	}
	if c.Payments.Provider != "stripe" {
		return nil
	}
	if strings.TrimSpace(c.Payments.Stripe.SecretKey) == "" {
		return fmt.Errorf("config: stripe.secret_key: required when payments.provider is stripe")
	}
	if strings.TrimSpace(c.Payments.Stripe.WebhookSecret) == "" {
		return fmt.Errorf("config: stripe.webhook_secret: required when payments.provider is stripe")
	}
	if strings.TrimSpace(c.Payments.Stripe.PriceID) == "" {
		return fmt.Errorf("config: stripe.price_id: required when payments.provider is stripe")
	}
	if c.HTTP.PublicURL == "" {
		return fmt.Errorf("config: http.public_url is required when payments.provider is stripe (checkout redirect URLs)")
	}
	return nil
}

// validateScheduler rejects zero/negative timing knobs and malformed opening windows.
// It is unconditional: a disabled scheduler still holds values that a future start would
// trust, and the defaults keep a no-env boot valid.
func (c Config) validateScheduler() error {
	s := c.Scheduler
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"scheduler.tick", s.Tick},
		{"scheduler.lease", s.Lease},
		{"scheduler.busy_backoff", s.BusyBackoff},
		{"scheduler.no_answer_backoff", s.NoAnswerBackoff},
		{"scheduler.error_backoff", s.ErrorBackoff},
	} {
		if d.value <= 0 {
			return fmt.Errorf("config: %s: must be positive", d.name)
		}
	}
	if s.MaxAttempts < 1 {
		return fmt.Errorf("config: scheduler.max_attempts: must be at least 1")
	}
	if s.MaxConcurrent < 1 {
		return fmt.Errorf("config: scheduler.max_concurrent: must be at least 1")
	}
	if err := validateWindowsJSON(s.WindowsJSON); err != nil {
		return err
	}
	return nil
}

type windowSetting struct {
	Dow   string `json:"dow"`
	Start string `json:"start"`
	End   string `json:"end"`
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// validateWindowsJSON parses the opening-window array independently of the scheduler
// package, so config stays a dependency-free, fail-fast surface.
func validateWindowsJSON(raw string) error {
	var windows []windowSetting
	if err := json.Unmarshal([]byte(raw), &windows); err != nil {
		return fmt.Errorf("config: scheduler.windows: invalid JSON: %w", err)
	}
	if len(windows) == 0 {
		return fmt.Errorf("config: scheduler.windows: at least one window is required")
	}
	for i, w := range windows {
		if _, ok := weekdayNames[strings.ToLower(w.Dow)]; !ok {
			return fmt.Errorf("config: scheduler.windows[%d].dow: unknown day %q", i, w.Dow)
		}
		start, err := parseClockSetting(w.Start)
		if err != nil {
			return fmt.Errorf("config: scheduler.windows[%d].start: %w", i, err)
		}
		end, err := parseClockSetting(w.End)
		if err != nil {
			return fmt.Errorf("config: scheduler.windows[%d].end: %w", i, err)
		}
		if start >= end {
			return fmt.Errorf("config: scheduler.windows[%d]: start must be before end", i)
		}
	}
	return nil
}

func parseClockSetting(raw string) (int, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	return hour*60 + minute, nil
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
	if (d < 80 || d > 1200) && d != 2400 {
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

// redacted reports only whether a secret is configured, never its value.
func redacted(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<unset>"
	}
	return "<redacted>"
}

// Summary returns non-secret values for startup logging and /healthz.
// NEVER include AccountSID, AuthToken, APIKey, or the Postgres DSN.
func (c Config) Summary() map[string]any {
	return map[string]any{
		"http.addr":                             c.HTTP.Addr,
		"http.public_url":                       c.HTTP.PublicURL,
		"telephony.provider":                    c.Telephony.Provider,
		"telephony.twilio.mode":                 c.Telephony.Twilio.Mode,
		"telephony.twilio.ring_timeout_seconds": c.Telephony.Twilio.RingTimeoutSeconds,
		"telephony.telnyx.connection_id":        c.Telephony.Telnyx.ConnectionID,
		"telephony.telnyx.from_number":          c.Telephony.Telnyx.FromNumber,
		"telephony.telnyx.ring_timeout_seconds": c.Telephony.Telnyx.RingTimeoutSeconds,
		"telnyx.api_key":                        redacted(c.Telephony.Telnyx.APIKey),
		"stt.provider":                          c.STT.Provider,
		"tts.provider":                          c.TTS.Provider,
		"dialogue.provider":                     c.Dialogue.Provider,
		"openai.base_url":                       c.OpenAI.BaseURL,
		"openai.dialogue_model":                 c.OpenAI.DialogueModel,
		"openai.api_key":                        redacted(c.OpenAI.APIKey),
		"notify.provider":                       c.Notify.Provider,
		"notify.webhook.channel":                c.Notify.Webhook.Channel,
		"notify.webhook.url":                    redacted(c.Notify.Webhook.URL),
		"storage.provider":                      c.Storage.Provider,
		"storage.local.dir":                     c.Storage.Local.Dir,
		"postgres.dsn":                          redacted(c.Storage.Postgres.DSN),
		"scheduler.enabled":                     c.Scheduler.Enabled,
		"scheduler.tick":                        c.Scheduler.Tick.String(),
		"scheduler.max_attempts":                c.Scheduler.MaxAttempts,
		"scheduler.max_concurrent":              c.Scheduler.MaxConcurrent,
		"scheduler.lease":                       c.Scheduler.Lease.String(),
		"scheduler.busy_backoff":                c.Scheduler.BusyBackoff.String(),
		"scheduler.no_answer_backoff":           c.Scheduler.NoAnswerBackoff.String(),
		"scheduler.error_backoff":               c.Scheduler.ErrorBackoff.String(),
		"scheduler.windows":                     c.Scheduler.WindowsJSON,
		"api.idempotency_ttl":                   c.API.IdempotencyTTL.String(),
		"auth.mode":                             c.Auth.Mode,
		"auth.session_ttl":                      c.Auth.SessionTTL.String(),
		"auth.cookie_name":                      c.Auth.CookieName,
		"auth.bcrypt_cost":                      c.Auth.BcryptCost,
		"payments.provider":                     c.Payments.Provider,
		"stripe.secret_key":                     redacted(c.Payments.Stripe.SecretKey),
		"stripe.webhook_secret":                 redacted(c.Payments.Stripe.WebhookSecret),
		"stripe.price_id":                       c.Payments.Stripe.PriceID,
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
