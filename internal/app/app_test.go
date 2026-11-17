package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		HTTP:      config.HTTP{Addr: "127.0.0.1:0"},
		Telephony: config.Telephony{Provider: "stub"},
		STT:       config.STT{Provider: "stub"},
		TTS:       config.TTS{Provider: "stub"},
		Dialogue:  config.Dialogue{Provider: "stub"},
		Storage:   config.Storage{Provider: "local", Local: config.Local{Dir: t.TempDir()}},
		Log:       config.Log{Level: "debug", Format: "text"},
	}
}

func TestNewWiresDefaults(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Storage == nil || a.Telephony == nil || a.STT == nil || a.TTS == nil || a.Dialogue == nil {
		t.Fatalf("nil adapter in %+v", a)
	}
	if a.Payments != nil || a.Notify != nil {
		t.Error("payments/notify must stay nil until C6/C7")
	}
	if a.Log == nil {
		t.Error("logger is nil")
	}
	if a.Config.Storage.Provider != "local" {
		t.Errorf("config not retained: %+v", a.Config)
	}
}

func TestNewRejectsUnimplementedProviders(t *testing.T) {
	// Every mistral-backed port must fail until C2.
	for name, mutate := range map[string]func(*config.Config){
		"stt":      func(c *config.Config) { c.STT.Provider = "mistral" },
		"tts":      func(c *config.Config) { c.TTS.Provider = "mistral" },
		"dialogue": func(c *config.Config) { c.Dialogue.Provider = "mistral" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			mutate(&cfg)
			if _, err := New(cfg); !errors.Is(err, ports.ErrNotImplemented) {
				t.Fatalf("err = %v, want ErrNotImplemented", err)
			}
		})
	}
}

func TestNewBuildsTwilio(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.PublicURL = "https://example.test"
	cfg.Telephony = config.Telephony{
		Provider: "twilio",
		Twilio: config.Twilio{
			AccountSID:         "AC" + strings.Repeat("0", 32),
			AuthToken:          "test-token",
			PhoneNumber:        "+15551234567",
			Mode:               "voice-api",
			RingTimeoutSeconds: 30,
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Telephony == nil {
		t.Fatal("telephony adapter not wired")
	}
	if a.Webhooks == nil {
		t.Fatal("twilio webhooks handler not wired")
	}
}

func TestNewTwilioSIPTrunkNotImplemented(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.PublicURL = "https://example.test"
	cfg.Telephony = config.Telephony{
		Provider: "twilio",
		Twilio: config.Twilio{
			AccountSID:         "AC" + strings.Repeat("0", 32),
			AuthToken:          "test-token",
			PhoneNumber:        "+15551234567",
			Mode:               "sip-trunk",
			RingTimeoutSeconds: 30,
		},
	}
	if _, err := New(cfg); !errors.Is(err, ports.ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
}

func TestStubHasNoWebhooks(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Webhooks != nil {
		t.Fatal("stub provider must not mount webhooks")
	}
}

func TestNewRejectsUnknownProviders(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.Provider = "postgres"
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for unknown storage provider")
	}
}

func TestNewFailsOnUnwritableStorage(t *testing.T) {
	cfg := testConfig(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// MkdirAll must fail because a regular file is in the way.
	cfg.Storage.Local.Dir = filepath.Join(blocker, "storage")
	if _, err := New(cfg); err == nil {
		t.Fatal("expected storage construction error")
	}
}

func TestParseLevelFallback(t *testing.T) {
	if got := parseLevel("bogus"); got.String() != "INFO" {
		t.Errorf("parseLevel(bogus) = %s, want INFO", got)
	}
	if got := parseLevel("warn"); got.String() != "WARN" {
		t.Errorf("parseLevel(warn) = %s, want WARN", got)
	}
}
