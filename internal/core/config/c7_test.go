package config

import (
	"strings"
	"testing"
)

const (
	c7TelnyxKey   = "KEY0123456789abcdef"
	c7PublicURL   = "https://example.trycloudflare.com"
	c7OpenAIKey   = "sk-test-openai"
	c7WebhookURL  = "https://hooks.example.test/insteadly"
	c7TelnyxEnvOK = "INSTEADLY_TELNYX_CONNECTION_ID=conn_123"
)

func telnyxEnv() []string {
	return []string{
		"INSTEADLY_TELEPHONY_PROVIDER=telnyx",
		"INSTEADLY_HTTP_PUBLIC_URL=" + c7PublicURL,
		"INSTEADLY_TELNYX_API_KEY=" + c7TelnyxKey,
		c7TelnyxEnvOK,
		"INSTEADLY_TELNYX_FROM_NUMBER=+15551234567",
	}
}

func TestC7Defaults(t *testing.T) {
	cfg, err := load("", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Telephony.Telnyx.APIKey != "" || cfg.Telephony.Telnyx.ConnectionID != "" ||
		cfg.Telephony.Telnyx.FromNumber != "" || cfg.Telephony.Telnyx.RingTimeoutSeconds != 0 {
		t.Errorf("telnyx defaults = %+v", cfg.Telephony.Telnyx)
	}
	if cfg.OpenAI.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("openai.base_url = %q", cfg.OpenAI.BaseURL)
	}
	if cfg.OpenAI.DialogueModel != "gpt-4o-mini" {
		t.Errorf("openai.dialogue_model = %q", cfg.OpenAI.DialogueModel)
	}
	if cfg.OpenAI.APIKey != "" {
		t.Error("openai api key must default empty")
	}
	if cfg.Notify.Provider != "off" {
		t.Errorf("notify.provider = %q, want off", cfg.Notify.Provider)
	}
	if cfg.Notify.Webhook.Channel != "email" {
		t.Errorf("notify.webhook.channel = %q, want email", cfg.Notify.Webhook.Channel)
	}
	if cfg.Notify.Webhook.URL != "" {
		t.Error("notify webhook url must default empty")
	}
}

func TestTelnyxEnvParsed(t *testing.T) {
	cfg, err := load("", append(telnyxEnv(), "INSTEADLY_TELNYX_RING_TIMEOUT_SECONDS=45"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tel := cfg.Telephony.Telnyx
	if tel.APIKey != c7TelnyxKey {
		t.Error("telnyx api key not parsed")
	}
	if tel.ConnectionID != "conn_123" {
		t.Errorf("connection_id = %q", tel.ConnectionID)
	}
	if tel.FromNumber != "+15551234567" {
		t.Errorf("from_number = %q", tel.FromNumber)
	}
	if tel.RingTimeoutSeconds != 45 {
		t.Errorf("ring_timeout_seconds = %d", tel.RingTimeoutSeconds)
	}
}

func TestTelnyxValidation(t *testing.T) {
	cases := map[string]string{
		"missing api key":    `INSTEADLY_TELNYX_API_KEY=`,
		"missing connection": `INSTEADLY_TELNYX_CONNECTION_ID=`,
		"empty connection":   "INSTEADLY_TELNYX_CONNECTION_ID=   ",
		"bad from number":    "INSTEADLY_TELNYX_FROM_NUMBER=0664 1234567",
		"empty from number":  "INSTEADLY_TELNYX_FROM_NUMBER=",
	}
	replace := func(env []string, kv string) []string {
		name, _, _ := strings.Cut(kv, "=")
		out := env[:0]
		for _, e := range env {
			if strings.HasPrefix(e, name+"=") {
				continue
			}
			out = append(out, e)
		}
		return append(out, kv)
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load("", replace(telnyxEnv(), kv))
			if err == nil {
				t.Fatalf("expected error for %q", kv)
			}
		})
	}

	// A public URL is mandatory: webhooks are how Telnyx reports call progress.
	_, err := load("", []string{
		"INSTEADLY_TELEPHONY_PROVIDER=telnyx",
		"INSTEADLY_TELNYX_API_KEY=" + c7TelnyxKey,
		c7TelnyxEnvOK,
		"INSTEADLY_TELNYX_FROM_NUMBER=+15551234567",
	})
	if err == nil || !strings.Contains(err.Error(), "http.public_url") {
		t.Fatalf("err = %v, want http.public_url error", err)
	}
}

func TestTelnyxRingTimeoutRange(t *testing.T) {
	for _, v := range []string{"0", "5", "600"} {
		env := append(telnyxEnv(), "INSTEADLY_TELNYX_RING_TIMEOUT_SECONDS="+v)
		if _, err := load("", env); err != nil {
			t.Fatalf("ring timeout %s rejected: %v", v, err)
		}
	}
	for _, v := range []string{"4", "601", "-1", "nope"} {
		env := append(telnyxEnv(), "INSTEADLY_TELNYX_RING_TIMEOUT_SECONDS="+v)
		_, err := load("", env)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ring_timeout_seconds") {
			t.Fatalf("ring timeout %s err = %v", v, err)
		}
	}
}

// telnyx settings are validated unconditionally when explicitly set, but only required
// when the telnyx provider is selected.
func TestTelnyxSettingsOptionalForOtherProviders(t *testing.T) {
	if _, err := load("", []string{"INSTEADLY_TELNYX_CONNECTION_ID=conn_123"}); err != nil {
		t.Fatalf("telnyx settings set while provider is stub must be allowed: %v", err)
	}
}

func TestOpenAIEnvParsed(t *testing.T) {
	cfg, err := load("", []string{
		"INSTEADLY_DIALOGUE_PROVIDER=openai",
		"INSTEADLY_OPENAI_API_KEY=" + c7OpenAIKey,
		"INSTEADLY_OPENAI_BASE_URL=https://api.example.test/v1/",
		"INSTEADLY_OPENAI_DIALOGUE_MODEL=gpt-test",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.OpenAI.APIKey != c7OpenAIKey {
		t.Error("openai api key not parsed")
	}
	if cfg.OpenAI.BaseURL != "https://api.example.test/v1" {
		t.Errorf("base_url = %q, want trailing slash trimmed", cfg.OpenAI.BaseURL)
	}
	if cfg.OpenAI.DialogueModel != "gpt-test" {
		t.Errorf("dialogue_model = %q", cfg.OpenAI.DialogueModel)
	}
}

func TestOpenAIAPIKeyRequiredForOpenAIProvider(t *testing.T) {
	_, err := load("", []string{"INSTEADLY_DIALOGUE_PROVIDER=openai"})
	if err == nil || !strings.Contains(err.Error(), "openai.api_key") {
		t.Fatalf("err = %v, want openai.api_key error", err)
	}
	// Optional while every dialogue provider is stub/mistral.
	if _, err := load("", nil); err != nil {
		t.Fatalf("stub dialogue must not require an openai key: %v", err)
	}
}

func TestOpenAIBaseURLValidation(t *testing.T) {
	valid := []string{
		"https://api.openai.com/v1",
		"https://api.openai.com/v1/",
		"https://api.example.test",
		"https://example.test:8443/v1",
	}
	for _, raw := range valid {
		t.Run("valid "+raw, func(t *testing.T) {
			if _, err := load("", []string{"INSTEADLY_OPENAI_BASE_URL=" + raw}); err != nil {
				t.Fatalf("load(%q): %v", raw, err)
			}
		})
	}
	invalid := []string{
		"http://api.openai.com/v1", "https://", "https://user:pass@api.openai.com/v1",
		"https://api.openai.com/v1?x=1", "https://api.openai.com/v1#frag", "",
	}
	for _, raw := range invalid {
		t.Run("invalid "+raw, func(t *testing.T) {
			_, err := load("", []string{"INSTEADLY_OPENAI_BASE_URL=" + raw})
			if err == nil || !strings.Contains(err.Error(), "openai.base_url") {
				t.Fatalf("load(%q) err = %v, want openai.base_url error", raw, err)
			}
		})
	}
}

func TestOpenAIModelRequired(t *testing.T) {
	for _, kv := range []string{"INSTEADLY_OPENAI_DIALOGUE_MODEL=", "INSTEADLY_OPENAI_DIALOGUE_MODEL= gpt-4o-mini "} {
		_, err := load("", []string{kv})
		if err == nil || !strings.Contains(err.Error(), "openai.dialogue_model") {
			t.Fatalf("load(%q) err = %v, want openai.dialogue_model error", kv, err)
		}
	}
}

func TestDialogueProviderAllowsOpenAI(t *testing.T) {
	if _, err := load("", []string{
		"INSTEADLY_DIALOGUE_PROVIDER=openai",
		"INSTEADLY_OPENAI_API_KEY=" + c7OpenAIKey,
	}); err != nil {
		t.Fatalf("openai dialogue rejected: %v", err)
	}
	// The alternative provider is Dialogue-only in C7: STT/TTS keep the narrow enum.
	if _, err := load("", []string{"INSTEADLY_STT_PROVIDER=openai"}); err == nil {
		t.Fatal("stt.provider=openai must be rejected")
	}
	if _, err := load("", []string{"INSTEADLY_TTS_PROVIDER=openai"}); err == nil {
		t.Fatal("tts.provider=openai must be rejected")
	}
}

func TestNotifyEnvParsed(t *testing.T) {
	cfg, err := load("", []string{
		"INSTEADLY_NOTIFY_PROVIDER=webhook",
		"INSTEADLY_NOTIFY_WEBHOOK_URL=" + c7WebhookURL,
		"INSTEADLY_NOTIFY_WEBHOOK_CHANNEL=sms",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Notify.Provider != "webhook" || cfg.Notify.Webhook.URL != c7WebhookURL ||
		cfg.Notify.Webhook.Channel != "sms" {
		t.Errorf("notify = %+v", cfg.Notify)
	}
}

func TestNotifyValidation(t *testing.T) {
	cases := map[string]string{
		"unknown provider": "INSTEADLY_NOTIFY_PROVIDER=email",
		"bad channel":      "INSTEADLY_NOTIFY_WEBHOOK_CHANNEL=push",
		"webhook no url":   "INSTEADLY_NOTIFY_PROVIDER=webhook",
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load("", []string{kv}); err == nil {
				t.Fatalf("expected error for %q", kv)
			}
		})
	}
	for _, provider := range []string{"off", "log"} {
		t.Run(provider, func(t *testing.T) {
			if _, err := load("", []string{"INSTEADLY_NOTIFY_PROVIDER=" + provider}); err != nil {
				t.Fatalf("notify provider %q rejected: %v", provider, err)
			}
		})
	}
}

func TestC7SummaryNeverLeaksValues(t *testing.T) {
	cfg, err := load("", append(telnyxEnv(),
		"INSTEADLY_OPENAI_API_KEY="+c7OpenAIKey,
		"INSTEADLY_NOTIFY_PROVIDER=webhook",
		"INSTEADLY_NOTIFY_WEBHOOK_URL="+c7WebhookURL,
	))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	summary := cfg.Summary()
	for _, key := range []string{"telnyx.api_key", "openai.api_key", "notify.webhook.url"} {
		if got := summary[key]; got != "<redacted>" {
			t.Errorf("%s = %v, want <redacted>", key, got)
		}
	}
	for _, secret := range []string{c7TelnyxKey, c7OpenAIKey, c7WebhookURL} {
		for k, v := range summary {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Errorf("summary key %q leaks a secret", k)
			}
		}
	}
	// Non-secret identifiers remain visible for ops.
	if summary["telephony.telnyx.connection_id"] != "conn_123" {
		t.Errorf("connection_id = %v, want the id visible", summary["telephony.telnyx.connection_id"])
	}
	if summary["notify.provider"] != "webhook" || summary["notify.webhook.channel"] != "email" {
		t.Errorf("notify summary = %v / %v", summary["notify.provider"], summary["notify.webhook.channel"])
	}
}
