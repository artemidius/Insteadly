package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDotEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:8080" {
		t.Errorf("http.addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Telephony.Provider != "stub" || cfg.STT.Provider != "stub" ||
		cfg.TTS.Provider != "stub" || cfg.Dialogue.Provider != "stub" {
		t.Errorf("default providers are not stub: %+v", cfg)
	}
	if cfg.Storage.Provider != "local" || cfg.Storage.Local.Dir != "./data" {
		t.Errorf("storage defaults = %+v", cfg.Storage)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("log defaults = %+v", cfg.Log)
	}
	if cfg.Recording.RecordingEnabled || cfg.Recording.TranscriptEnabled || cfg.Recording.AudioEnabled {
		t.Errorf("recording flags should default to false: %+v", cfg.Recording)
	}
}

func TestDotEnvApplied(t *testing.T) {
	path := writeDotEnv(t, `
# comment line
INSTEADLY_HTTP_ADDR = 127.0.0.1:9999
INSTEADLY_LOG_LEVEL=debug
INSTEADLY_STORAGE_LOCAL_DIR=/tmp/insteadly-test
INSTEADLY_RECORDING_ENABLED=true
TWILIO_ACCOUNT_SID=ignored-because-unprefixed
`)
	cfg, err := load(path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:9999" {
		t.Errorf("http.addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q", cfg.Log.Level)
	}
	if cfg.Storage.Local.Dir != "/tmp/insteadly-test" {
		t.Errorf("storage.local.dir = %q", cfg.Storage.Local.Dir)
	}
	if !cfg.Recording.RecordingEnabled {
		t.Error("recording.enabled not applied")
	}
}

func TestMissingDotEnvIsFine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.env")
	if _, err := load(path, nil); err != nil {
		t.Fatalf("load with missing .env: %v", err)
	}
}

func TestEnvironWinsOverDotEnv(t *testing.T) {
	path := writeDotEnv(t, "INSTEADLY_LOG_LEVEL=debug\n")
	cfg, err := load(path, []string{"INSTEADLY_LOG_LEVEL=warn"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn (env must beat .env)", cfg.Log.Level)
	}
}

func TestUnknownVariableRejected(t *testing.T) {
	t.Run("environ", func(t *testing.T) {
		_, err := load("", []string{"INSTEADLY_NOPE=1"})
		if err == nil || !strings.Contains(err.Error(), "unknown variable") {
			t.Fatalf("err = %v, want unknown-variable error", err)
		}
	})
	t.Run("dotenv", func(t *testing.T) {
		path := writeDotEnv(t, "INSTEADLY_TYPO=true\n")
		_, err := load(path, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown variable") {
			t.Fatalf("err = %v, want unknown-variable error", err)
		}
	})
}

func TestNonInsteadlyEnvIgnored(t *testing.T) {
	cfg, err := load("", []string{"TWILIO_ACCOUNT_SID=abc", "PATH=/usr/bin", "BOGUS=1"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Telephony.Twilio.AccountSID != "" {
		t.Error("unprefixed TWILIO_ACCOUNT_SID must not be read in C0")
	}
}

func TestEnumViolations(t *testing.T) {
	cases := map[string]string{
		"telephony.provider": "INSTEADLY_TELEPHONY_PROVIDER=fonira",
		"stt.provider":       "INSTEADLY_STT_PROVIDER=whisper",
		"tts.provider":       "INSTEADLY_TTS_PROVIDER=elevenlabs",
		"dialogue.provider":  "INSTEADLY_DIALOGUE_PROVIDER=gpt",
		"storage.provider":   "INSTEADLY_STORAGE_PROVIDER=bogus",
		"log.level":          "INSTEADLY_LOG_LEVEL=verbose",
		"log.format":         "INSTEADLY_LOG_FORMAT=xml",
		"twilio.mode":        "INSTEADLY_TWILIO_MODE=api",
		"http.addr":          "INSTEADLY_HTTP_ADDR=",
	}
	for field, kv := range cases {
		t.Run(field, func(t *testing.T) {
			_, err := load("", []string{kv})
			if err == nil {
				t.Fatalf("expected validation error for %s", kv)
			}
			if !strings.Contains(err.Error(), "config:") {
				t.Errorf("error %q is not a config error", err)
			}
		})
	}
}

func TestBadBoolRejected(t *testing.T) {
	_, err := load("", []string{"INSTEADLY_RECORDING_ENABLED=yes"})
	if err == nil || !strings.Contains(err.Error(), "expected true/false") {
		t.Fatalf("err = %v, want bool parse error", err)
	}
	if strings.Contains(err.Error(), "INSTEADLY_RECORDING_ENABLED:") == false {
		t.Logf("note: error does not name the variable: %v", err)
	}
}

func TestMalformedDotEnvLine(t *testing.T) {
	path := writeDotEnv(t, "INSTEADLY_LOG_LEVEL\n")
	_, err := load(path, nil)
	if err == nil || !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("err = %v, want parse error", err)
	}
}

func TestSummaryContainsNoSecrets(t *testing.T) {
	const (
		sid           = "AC-super-secret-sid"
		token         = "super-secret-token"
		key           = "sk-super-secret-key"
		dsn           = "postgres://insteadly:super-secret-dsn-password@db.example.test:5432/insteadly"
		apiKey        = "super-secret-api-key"
		stripeKey     = "sk_live_super_secret_stripe_key"
		stripeWebhook = "whsec_super_secret_webhook"
	)
	cfg, err := load("", []string{
		"INSTEADLY_TWILIO_ACCOUNT_SID=" + sid,
		"INSTEADLY_TWILIO_AUTH_TOKEN=" + token,
		"INSTEADLY_MISTRAL_API_KEY=" + key,
		"INSTEADLY_POSTGRES_DSN=" + dsn,
		"INSTEADLY_API_KEY=" + apiKey,
		"INSTEADLY_STRIPE_SECRET_KEY=" + stripeKey,
		"INSTEADLY_STRIPE_WEBHOOK_SECRET=" + stripeWebhook,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	summary := cfg.Summary()
	wantKeys := []string{
		"http.addr", "http.public_url", "telephony.provider", "telephony.twilio.mode",
		"telephony.twilio.ring_timeout_seconds", "stt.provider", "tts.provider",
		"dialogue.provider", "storage.provider", "storage.local.dir",
		"recording.enabled", "transcript.enabled", "audio.enabled",
		"log.level", "log.format",
		"mistral.api_base_url", "mistral.stt.realtime_model", "mistral.stt.batch_model",
		"mistral.stt.streaming_delay_ms", "mistral.tts.model", "mistral.tts.voice",
		"mistral.tts.mode", "mistral.dialogue.model",
		"postgres.dsn", "scheduler.enabled", "scheduler.tick", "scheduler.max_attempts",
		"scheduler.max_concurrent", "scheduler.lease", "scheduler.busy_backoff",
		"scheduler.no_answer_backoff", "scheduler.error_backoff", "scheduler.windows",
		"api.idempotency_ttl",
		"auth.mode", "auth.session_ttl", "auth.cookie_name", "auth.bcrypt_cost",
		"payments.provider", "stripe.secret_key", "stripe.webhook_secret", "stripe.price_id",
	}
	for _, k := range wantKeys {
		if _, ok := summary[k]; !ok {
			t.Errorf("summary is missing %q", k)
		}
	}
	if len(summary) != len(wantKeys) {
		t.Errorf("summary has %d keys, want %d: %v", len(summary), len(wantKeys), summary)
	}

	for k, v := range summary {
		s := strings.ToLower(k)
		str, _ := v.(string)
		if strings.Contains(s, "sid") || strings.Contains(s, "token") ||
			strings.Contains(s, "key") || strings.Contains(s, "secret") {
			if str != "<redacted>" && str != "<unset>" {
				t.Errorf("summary key %q looks secret but is not redacted: %v", k, v)
			}
		}
		for _, secret := range []string{sid, token, key, dsn, apiKey, stripeKey, stripeWebhook, "super-secret-dsn-password"} {
			if strings.Contains(str, secret) {
				t.Errorf("summary key %q leaks a secret value", k)
			}
		}
	}

	for _, key := range []string{"postgres.dsn", "stripe.secret_key", "stripe.webhook_secret"} {
		if got := summary[key]; got != "<redacted>" {
			t.Errorf("%s = %v, want <redacted>", key, got)
		}
	}
	// The secret flag in the registry is what documents which names are sensitive;
	// make sure the test suite actually covers those entries.
	for name, s := range registry {
		if !s.secret {
			continue
		}
		if !strings.Contains(name, "SID") && !strings.Contains(name, "TOKEN") &&
			!strings.Contains(name, "KEY") && !strings.Contains(name, "DSN") &&
			!strings.Contains(name, "SECRET") {
			t.Errorf("registry entry %q is marked secret but its name does not look secret", name)
		}
	}
}

func TestPublicURLValidation(t *testing.T) {
	valid := []string{
		"", "https://abc.trycloudflare.com", "https://example.test:8443",
		"https://example.test/",
	}
	for _, raw := range valid {
		t.Run("valid "+raw, func(t *testing.T) {
			if _, err := load("", []string{"INSTEADLY_HTTP_PUBLIC_URL=" + raw}); err != nil {
				t.Fatalf("load(%q): %v", raw, err)
			}
		})
	}

	invalid := []string{
		"http://example.test", "https://", "https://example.test/webhooks",
		"https://user:pass@example.test", "https://example.test?x=1",
		"https://example.test#frag",
	}
	for _, raw := range invalid {
		t.Run("invalid "+raw, func(t *testing.T) {
			_, err := load("", []string{"INSTEADLY_HTTP_PUBLIC_URL=" + raw})
			if err == nil || !strings.Contains(err.Error(), "http.public_url") {
				t.Fatalf("load(%q) err = %v, want public_url error", raw, err)
			}
		})
	}
}

func TestPublicURLTrailingSlashTrimmed(t *testing.T) {
	cfg, err := load("", []string{"INSTEADLY_HTTP_PUBLIC_URL=https://example.test/"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.PublicURL != "https://example.test" {
		t.Errorf("public_url = %q, want no trailing slash", cfg.HTTP.PublicURL)
	}
}

func twilioEnv() []string {
	return []string{
		"INSTEADLY_TELEPHONY_PROVIDER=twilio",
		"INSTEADLY_HTTP_PUBLIC_URL=https://example.test",
		"INSTEADLY_TWILIO_ACCOUNT_SID=AC" + strings.Repeat("a", 32),
		"INSTEADLY_TWILIO_AUTH_TOKEN=test-token",
		"INSTEADLY_TWILIO_PHONE_NUMBER=+15551234567",
	}
}

func TestTwilioValidation(t *testing.T) {
	if _, err := load("", twilioEnv()); err != nil {
		t.Fatalf("valid twilio config rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func([]string) []string
		want   string
	}{
		"missing public url": {
			mutate: func(env []string) []string {
				return removeEnv(env, "INSTEADLY_HTTP_PUBLIC_URL")
			},
			want: "http.public_url is required",
		},
		"bad sid": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_TWILIO_ACCOUNT_SID=AC123")
			},
			want: "account_sid",
		},
		"missing token": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_TWILIO_AUTH_TOKEN=")
			},
			want: "auth_token",
		},
		"bad phone": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_TWILIO_PHONE_NUMBER=555-1234")
			},
			want: "phone_number",
		},
		"bad ring timeout": {
			mutate: func(env []string) []string {
				return append(env, "INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS=4")
			},
			want: "ring_timeout_seconds",
		},
		"bad mode": {
			mutate: func(env []string) []string {
				return append(env, "INSTEADLY_TWILIO_MODE=api")
			},
			want: "telephony.twilio.mode",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load("", tc.mutate(twilioEnv()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSIPTrunkDoesNotRequirePublicURL(t *testing.T) {
	env := removeEnv(twilioEnv(), "INSTEADLY_HTTP_PUBLIC_URL")
	env = append(env, "INSTEADLY_TWILIO_MODE=sip-trunk")
	if _, err := load("", env); err != nil {
		t.Fatalf("sip-trunk without public URL rejected: %v", err)
	}
}

func removeEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func replaceEnv(env []string, kv string) []string {
	name, _, _ := strings.Cut(kv, "=")
	out := removeEnv(env, name)
	return append(out, kv)
}

func TestBoolValuesAccepted(t *testing.T) {
	cfg, err := load("", []string{
		"INSTEADLY_RECORDING_ENABLED=true",
		"INSTEADLY_TRANSCRIPT_ENABLED=1",
		"INSTEADLY_AUDIO_ENABLED=false",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Recording.RecordingEnabled || !cfg.Recording.TranscriptEnabled || cfg.Recording.AudioEnabled {
		t.Errorf("recording flags = %+v", cfg.Recording)
	}
}

func TestMistralDefaults(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Mistral
	if m.BaseURL != "https://api.mistral.ai" {
		t.Errorf("base_url = %q", m.BaseURL)
	}
	if m.STT.RealtimeModel != "voxtral-mini-transcribe-realtime-3" ||
		m.STT.BatchModel != "voxtral-mini-transcribe-3" || m.STT.StreamingDelayMS != 480 {
		t.Errorf("stt defaults = %+v", m.STT)
	}
	if m.TTS.Model != "voxtral-mini-tts-3-0" || m.TTS.DefaultVoice != "voice_447" || m.TTS.Mode != "realtime" {
		t.Errorf("tts defaults = %+v", m.TTS)
	}
	if m.Dialogue.Model != "mistral-small-2603" {
		t.Errorf("dialogue model = %q", m.Dialogue.Model)
	}
	if m.APIKey != "" {
		t.Error("api key must default empty")
	}
}

func mistralProviderEnv() []string {
	return []string{
		"INSTEADLY_MISTRAL_API_KEY=test-key",
		"INSTEADLY_STT_PROVIDER=mistral",
		"INSTEADLY_TTS_PROVIDER=mistral",
		"INSTEADLY_DIALOGUE_PROVIDER=mistral",
	}
}

func TestMistralEnvOverrides(t *testing.T) {
	env := mistralProviderEnv()
	env = append(env,
		"INSTEADLY_MISTRAL_API_BASE_URL=https://api.example.test/",
		"INSTEADLY_MISTRAL_STT_REALTIME_MODEL=rt-model",
		"INSTEADLY_MISTRAL_STT_BATCH_MODEL=batch-model",
		"INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS=900",
		"INSTEADLY_MISTRAL_TTS_MODEL=tts-model",
		"INSTEADLY_MISTRAL_TTS_VOICE=voice_999",
		"INSTEADLY_MISTRAL_TTS_MODE=offline",
		"INSTEADLY_MISTRAL_DIALOGUE_MODEL=dialogue-model",
	)
	cfg, err := load("", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Mistral
	if m.BaseURL != "https://api.example.test" {
		t.Errorf("base_url = %q (want trailing slash trimmed)", m.BaseURL)
	}
	if m.STT.RealtimeModel != "rt-model" || m.STT.BatchModel != "batch-model" || m.STT.StreamingDelayMS != 900 {
		t.Errorf("stt = %+v", m.STT)
	}
	if m.TTS.Model != "tts-model" || m.TTS.DefaultVoice != "voice_999" || m.TTS.Mode != "offline" {
		t.Errorf("tts = %+v", m.TTS)
	}
	if m.Dialogue.Model != "dialogue-model" {
		t.Errorf("dialogue model = %q", m.Dialogue.Model)
	}
}

func TestMistralBaseURLValidation(t *testing.T) {
	valid := []string{
		"https://api.mistral.ai",
		"https://api.mistral.ai/",
		"https://example.test:8443",
	}
	for _, raw := range valid {
		t.Run("valid "+raw, func(t *testing.T) {
			if _, err := load("", []string{"INSTEADLY_MISTRAL_API_BASE_URL=" + raw}); err != nil {
				t.Fatalf("load(%q): %v", raw, err)
			}
		})
	}
	invalid := []string{
		"http://api.mistral.ai", "https://", "https://api.mistral.ai/v1",
		"https://user:pass@api.mistral.ai", "https://api.mistral.ai?x=1",
		"https://api.mistral.ai#frag", "",
	}
	for _, raw := range invalid {
		t.Run("invalid "+raw, func(t *testing.T) {
			_, err := load("", []string{"INSTEADLY_MISTRAL_API_BASE_URL=" + raw})
			if err == nil || !strings.Contains(err.Error(), "mistral.api_base_url") {
				t.Fatalf("load(%q) err = %v, want base_url error", raw, err)
			}
		})
	}
}

func TestMistralDelayValidation(t *testing.T) {
	accepted := []string{"80", "480", "1200", "2400"}
	for _, v := range accepted {
		t.Run("ok "+v, func(t *testing.T) {
			if _, err := load("", []string{"INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS=" + v}); err != nil {
				t.Fatalf("load(%s): %v", v, err)
			}
		})
	}
	rejected := []string{"79", "1201", "2401", "0", "not-an-int"}
	for _, v := range rejected {
		t.Run("bad "+v, func(t *testing.T) {
			_, err := load("", []string{"INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS=" + v})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "streaming_delay_ms") {
				t.Fatalf("load(%s) err = %v, want streaming_delay_ms error", v, err)
			}
		})
	}
}

func TestMistralModelAndVoiceValidation(t *testing.T) {
	cases := map[string]string{
		"stt realtime model empty": "INSTEADLY_MISTRAL_STT_REALTIME_MODEL=",
		"stt realtime model space": "INSTEADLY_MISTRAL_STT_REALTIME_MODEL= model ",
		"stt batch model empty":    "INSTEADLY_MISTRAL_STT_BATCH_MODEL=",
		"tts model empty":          "INSTEADLY_MISTRAL_TTS_MODEL=",
		"tts voice empty":          "INSTEADLY_MISTRAL_TTS_VOICE=",
		"tts voice space":          "INSTEADLY_MISTRAL_TTS_VOICE= voice_1 ",
		"dialogue model empty":     "INSTEADLY_MISTRAL_DIALOGUE_MODEL=",
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load("", []string{kv})
			if err == nil {
				t.Fatalf("expected error for %q", kv)
			}
		})
	}
}

func TestMistralTTSModeValidation(t *testing.T) {
	if _, err := load("", []string{"INSTEADLY_MISTRAL_TTS_MODE=offline"}); err != nil {
		t.Fatalf("offline mode rejected: %v", err)
	}
	_, err := load("", []string{"INSTEADLY_MISTRAL_TTS_MODE=hybrid"})
	if err == nil || !strings.Contains(err.Error(), "mistral.tts.mode") {
		t.Fatalf("err = %v, want tts.mode error", err)
	}
}

func TestMistralAPIKeyRequiredPerProvider(t *testing.T) {
	providers := []string{
		"INSTEADLY_STT_PROVIDER=mistral",
		"INSTEADLY_TTS_PROVIDER=mistral",
		"INSTEADLY_DIALOGUE_PROVIDER=mistral",
	}
	for _, kv := range providers {
		t.Run(kv, func(t *testing.T) {
			_, err := load("", []string{kv})
			if err == nil || !strings.Contains(err.Error(), "mistral.api_key") {
				t.Fatalf("err = %v, want mistral.api_key error", err)
			}
		})
	}
}

func TestMistralAPIKeyOptionalForAllStub(t *testing.T) {
	if _, err := load("", nil); err != nil {
		t.Fatalf("all-stub without key rejected: %v", err)
	}
}

func TestUnprefixedMistralKeyIgnored(t *testing.T) {
	cfg, err := load("", []string{
		"MISTRAL_API_KEY=unprefixed",
		"INSTEADLY_STT_PROVIDER=mistral",
	})
	if err == nil {
		t.Fatal("expected missing-key error: unprefixed key must not satisfy app config")
	} else if !strings.Contains(err.Error(), "mistral.api_key") {
		t.Fatalf("err = %v, want mistral.api_key error", err)
	}
	if cfg.Mistral.APIKey != "" {
		t.Error("unprefixed MISTRAL_API_KEY must not be read into app config")
	}
}

func TestSchedulerDefaults(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Scheduler
	if !s.Enabled {
		t.Error("scheduler must default to enabled")
	}
	if s.Tick.Seconds() != 5 || s.MaxAttempts != 3 || s.MaxConcurrent != 8 || s.Lease.Minutes() != 2 {
		t.Errorf("scheduler defaults = %+v", s)
	}
	if s.BusyBackoff.Minutes() != 15 || s.NoAnswerBackoff.Minutes() != 30 || s.ErrorBackoff.Minutes() != 5 {
		t.Errorf("backoff defaults = %+v", s)
	}
	if s.WindowsJSON != DefaultSchedulerWindows {
		t.Errorf("windows default = %q", s.WindowsJSON)
	}
	if cfg.API.IdempotencyTTL.Minutes() != 10 || cfg.API.Key != "" {
		t.Errorf("api defaults = %+v", cfg.API)
	}
	if cfg.Storage.Provider != "local" || cfg.Storage.Postgres.DSN != "" {
		t.Errorf("storage defaults = %+v", cfg.Storage)
	}
}

func TestSchedulerEnvOverrides(t *testing.T) {
	cfg, err := load("", []string{
		"INSTEADLY_SCHEDULER_ENABLED=false",
		"INSTEADLY_SCHEDULER_TICK=2s",
		"INSTEADLY_SCHEDULER_MAX_ATTEMPTS=4",
		"INSTEADLY_SCHEDULER_MAX_CONCURRENT=3",
		"INSTEADLY_SCHEDULER_LEASE=90s",
		"INSTEADLY_SCHEDULER_BUSY_BACKOFF=1m",
		"INSTEADLY_SCHEDULER_NO_ANSWER_BACKOFF=2m",
		"INSTEADLY_SCHEDULER_ERROR_BACKOFF=3m",
		"INSTEADLY_SCHEDULER_WINDOWS=[{\"dow\":\"sat\",\"start\":\"10:00\",\"end\":\"12:00\"}]",
		"INSTEADLY_API_IDEMPOTENCY_TTL=30s",
		"INSTEADLY_API_KEY=dev-key",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Scheduler
	if s.Enabled || s.Tick.Seconds() != 2 || s.MaxAttempts != 4 || s.MaxConcurrent != 3 {
		t.Errorf("scheduler = %+v", s)
	}
	if s.Lease.Seconds() != 90 || s.BusyBackoff.Minutes() != 1 || s.NoAnswerBackoff.Minutes() != 2 || s.ErrorBackoff.Minutes() != 3 {
		t.Errorf("durations = %+v", s)
	}
	if !strings.Contains(s.WindowsJSON, "sat") {
		t.Errorf("windows = %q", s.WindowsJSON)
	}
	if cfg.API.IdempotencyTTL.Seconds() != 30 || cfg.API.Key != "dev-key" {
		t.Errorf("api = %+v", cfg.API)
	}
}

func TestSchedulerValidation(t *testing.T) {
	cases := map[string]string{
		"zero tick":            "INSTEADLY_SCHEDULER_TICK=0s",
		"negative tick":        "INSTEADLY_SCHEDULER_TICK=-1s",
		"zero attempts":        "INSTEADLY_SCHEDULER_MAX_ATTEMPTS=0",
		"zero concurrent":      "INSTEADLY_SCHEDULER_MAX_CONCURRENT=0",
		"zero lease":           "INSTEADLY_SCHEDULER_LEASE=0s",
		"zero busy backoff":    "INSTEADLY_SCHEDULER_BUSY_BACKOFF=0s",
		"zero no-answer":       "INSTEADLY_SCHEDULER_NO_ANSWER_BACKOFF=0s",
		"zero error backoff":   "INSTEADLY_SCHEDULER_ERROR_BACKOFF=0s",
		"zero idempotency ttl": "INSTEADLY_API_IDEMPOTENCY_TTL=0s",
		"bad duration":         "INSTEADLY_SCHEDULER_TICK=soon",
		"invalid windows json": `INSTEADLY_SCHEDULER_WINDOWS=[{"dow":`,
		"empty windows":        `INSTEADLY_SCHEDULER_WINDOWS=[]`,
		"unknown dow":          `INSTEADLY_SCHEDULER_WINDOWS=[{"dow":"funday","start":"08:00","end":"18:00"}]`,
		"bad start":            `INSTEADLY_SCHEDULER_WINDOWS=[{"dow":"mon","start":"8:00","end":"18:00"}]`,
		"end after midnight":   `INSTEADLY_SCHEDULER_WINDOWS=[{"dow":"mon","start":"08:00","end":"24:00"}]`,
		"start not before end": `INSTEADLY_SCHEDULER_WINDOWS=[{"dow":"mon","start":"18:00","end":"08:00"}]`,
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load("", []string{kv}); err == nil {
				t.Fatalf("expected validation error for %s", kv)
			}
		})
	}
}

func TestPostgresProviderValidation(t *testing.T) {
	_, err := load("", []string{"INSTEADLY_STORAGE_PROVIDER=postgres"})
	if err == nil || !strings.Contains(err.Error(), "postgres.dsn") {
		t.Fatalf("err = %v, want a postgres.dsn error", err)
	}
	cfg, err := load("", []string{
		"INSTEADLY_STORAGE_PROVIDER=postgres",
		"INSTEADLY_POSTGRES_DSN=  postgres://insteadly@localhost/insteadly  ",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Storage.Postgres.DSN != "postgres://insteadly@localhost/insteadly" {
		t.Errorf("dsn = %q, want trimmed", cfg.Storage.Postgres.DSN)
	}
	if cfg.Storage.Local.Dir != "./data" {
		t.Errorf("local dir = %q, want the blob host dir", cfg.Storage.Local.Dir)
	}
}

func TestNoEnvStillBoots(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("no-env load: %v", err)
	}
	if cfg.Storage.Provider != "local" || !cfg.Scheduler.Enabled || cfg.API.Key != "" {
		t.Errorf("no-env config = %+v", cfg)
	}
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.Mode != "off" || cfg.Auth.SessionTTL != 24*time.Hour ||
		cfg.Auth.CookieName != "insteadly_session" || cfg.Auth.BcryptCost != 12 {
		t.Errorf("auth defaults = %+v", cfg.Auth)
	}
	if cfg.Payments.Provider != "stub" {
		t.Errorf("payments default = %q, want stub", cfg.Payments.Provider)
	}
	if cfg.Payments.Stripe.SecretKey != "" || cfg.Payments.Stripe.WebhookSecret != "" {
		t.Error("stripe secrets must default empty")
	}
}

func TestAuthEnvOverrides(t *testing.T) {
	cfg, err := load("", []string{
		"INSTEADLY_AUTH_MODE=cookie",
		"INSTEADLY_AUTH_SESSION_TTL=2h",
		"INSTEADLY_AUTH_COOKIE_NAME=insteadly_test",
		"INSTEADLY_AUTH_BCRYPT_COST=10",
		"INSTEADLY_PAYMENTS_PROVIDER=stub",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.Mode != "cookie" || cfg.Auth.SessionTTL != 2*time.Hour ||
		cfg.Auth.CookieName != "insteadly_test" || cfg.Auth.BcryptCost != 10 {
		t.Errorf("auth = %+v", cfg.Auth)
	}
}

func TestAuthValidation(t *testing.T) {
	cases := map[string]struct {
		kv   string
		want string
	}{
		"bad mode":     {"INSTEADLY_AUTH_MODE=jwt", "auth.mode"},
		"zero ttl":     {"INSTEADLY_AUTH_SESSION_TTL=0s", "auth.session_ttl"},
		"negative ttl": {"INSTEADLY_AUTH_SESSION_TTL=-1h", "auth.session_ttl"},
		"bad cookie":   {"INSTEADLY_AUTH_COOKIE_NAME=bad cookie", "auth.cookie_name"},
		"empty cookie": {"INSTEADLY_AUTH_COOKIE_NAME=", "auth.cookie_name"},
		"low cost":     {"INSTEADLY_AUTH_BCRYPT_COST=3", "auth.bcrypt_cost"},
		"high cost":    {"INSTEADLY_AUTH_BCRYPT_COST=32", "auth.bcrypt_cost"},
		"non-int cost": {"INSTEADLY_AUTH_BCRYPT_COST=high", "INSTEADLY_AUTH_BCRYPT_COST"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load("", []string{tc.kv})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func stripeEnv() []string {
	return []string{
		"INSTEADLY_PAYMENTS_PROVIDER=stripe",
		"INSTEADLY_HTTP_PUBLIC_URL=https://example.test",
		"INSTEADLY_STRIPE_SECRET_KEY=sk_test_123",
		"INSTEADLY_STRIPE_WEBHOOK_SECRET=whsec_test_123",
		"INSTEADLY_STRIPE_PRICE_ID=price_test_123",
	}
}

func TestPaymentsValidation(t *testing.T) {
	cfg, err := load("", stripeEnv())
	if err != nil {
		t.Fatalf("valid stripe config rejected: %v", err)
	}
	if cfg.Payments.Provider != "stripe" || cfg.Payments.Stripe.PriceID != "price_test_123" {
		t.Errorf("payments = %+v", cfg.Payments)
	}

	cases := map[string]struct {
		mutate func([]string) []string
		want   string
	}{
		"unknown provider": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_PAYMENTS_PROVIDER=paypal")
			},
			want: "payments.provider",
		},
		"missing secret key": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_STRIPE_SECRET_KEY=")
			},
			want: "stripe.secret_key",
		},
		"missing webhook secret": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_STRIPE_WEBHOOK_SECRET=")
			},
			want: "stripe.webhook_secret",
		},
		"missing price": {
			mutate: func(env []string) []string {
				return replaceEnv(env, "INSTEADLY_STRIPE_PRICE_ID=")
			},
			want: "stripe.price_id",
		},
		"missing public url": {
			mutate: func(env []string) []string {
				return removeEnv(env, "INSTEADLY_HTTP_PUBLIC_URL")
			},
			want: "http.public_url is required",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load("", tc.mutate(stripeEnv()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStripeValidationErrorsNeverLeakSecrets(t *testing.T) {
	env := removeEnv(stripeEnv(), "INSTEADLY_HTTP_PUBLIC_URL")
	_, err := load("", env)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	for _, secret := range []string{"sk_test_123", "whsec_test_123"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}
