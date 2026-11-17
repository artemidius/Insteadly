package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	if a.Agent == nil || a.Runner == nil {
		t.Fatal("agent/runner not wired")
	}
	if a.Scheduler == nil || a.Events == nil {
		t.Fatal("scheduler/events not wired")
	}
	if a.Payments == nil {
		t.Error("payments must be wired (stub is the default)")
	}
	if a.Auth == nil {
		t.Error("auth service must be wired")
	}
	if a.Notify != nil {
		t.Error("notify must stay nil until C7")
	}
	if a.Log == nil {
		t.Error("logger is nil")
	}
	if a.Config.Storage.Provider != "local" {
		t.Errorf("config not retained: %+v", a.Config)
	}
}

func mistralCfg(cfg *config.Config) {
	cfg.Mistral = config.Mistral{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		STT: config.MistralSTT{
			RealtimeModel:    "rt-model",
			BatchModel:       "batch-model",
			StreamingDelayMS: 480,
		},
		TTS:      config.MistralTTS{Model: "tts-model", DefaultVoice: "voice_1", Mode: "realtime"},
		Dialogue: config.MistralDialogue{Model: "dlg-model"},
	}
}

func TestNewWiresMistralProviders(t *testing.T) {
	providers := map[string]func(*config.Config){
		"stt":      func(c *config.Config) { c.STT.Provider = "mistral" },
		"tts":      func(c *config.Config) { c.TTS.Provider = "mistral" },
		"dialogue": func(c *config.Config) { c.Dialogue.Provider = "mistral" },
	}
	for name, mutate := range providers {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			mistralCfg(&cfg)
			mutate(&cfg)
			a, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			switch name {
			case "stt":
				if a.STT == nil {
					t.Fatal("stt not wired")
				}
			case "tts":
				if a.TTS == nil {
					t.Fatal("tts not wired")
				}
			case "dialogue":
				if a.Dialogue == nil {
					t.Fatal("dialogue not wired")
				}
			}
		})
	}

	// All three at once.
	cfg := testConfig(t)
	mistralCfg(&cfg)
	cfg.STT.Provider = "mistral"
	cfg.TTS.Provider = "mistral"
	cfg.Dialogue.Provider = "mistral"
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New(all mistral): %v", err)
	}
	if a.STT == nil || a.TTS == nil || a.Dialogue == nil {
		t.Fatal("mistral providers not wired")
	}
}

func TestNewRejectsMistralMissingKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.STT.Provider = "mistral"
	// Mistral.APIKey left empty; the adapter constructor must reject it without any
	// network I/O.
	if _, err := New(cfg); err == nil {
		t.Fatal("expected missing-key error at construction")
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

func TestAppCloseWithoutActiveCalls(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var nilApp *App
	if err := nilApp.Close(context.Background()); err != nil {
		t.Fatalf("nil App.Close: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close is part of the process-shutdown contract.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
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

func TestNewRejectsPostgresWithoutDSN(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.Provider = "postgres"
	// config.Validate would reject this first; app.New must not half-wire it either.
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for postgres without a DSN")
	}
}

func TestNewRejectsUnknownProviders(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.Provider = "bogus"
	if _, err := New(cfg); err == nil {
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

func TestAppStartCloseIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Scheduler.Enabled = true
	cfg.Scheduler.Tick = time.Millisecond
	cfg.Scheduler.WindowsJSON = config.DefaultSchedulerWindows
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	a.Start(ctx) // idempotent

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := a.CloseStorage(); err != nil {
		t.Fatalf("CloseStorage: %v", err)
	}
	if err := a.CloseStorage(); err != nil {
		t.Fatalf("second CloseStorage: %v", err)
	}
}

func TestAppStartNoopWhenSchedulerDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Scheduler.Enabled = false
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.Start(context.Background())
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type recordingCloser struct {
	mu     *sync.Mutex
	calls  *[]string
	closed bool
}

func (c *recordingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	*c.calls = append(*c.calls, "storage")
	return nil
}

func TestAppCloseDoesNotCloseStoragePool(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var mu sync.Mutex
	var calls []string
	closer := &recordingCloser{mu: &mu, calls: &calls}
	a.storageCloser = closer

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closer.closed {
		t.Fatal("App.Close closed the storage pool; it must be closed last by CloseStorage")
	}
	if err := a.CloseStorage(); err != nil {
		t.Fatalf("CloseStorage: %v", err)
	}
	if !closer.closed {
		t.Fatal("CloseStorage did not close the pool")
	}
	if len(calls) != 1 || calls[0] != "storage" {
		t.Errorf("closer calls = %v", calls)
	}
}

func TestBuildPasswordHasherValidatesCost(t *testing.T) {
	hasher, err := buildPasswordHasher(config.Auth{BcryptCost: 4}, nil)
	if err != nil || hasher == nil {
		t.Fatalf("buildPasswordHasher(4) = %v, %v", hasher, err)
	}
	for _, cost := range []int{3, 32, 99} {
		if _, err := buildPasswordHasher(config.Auth{BcryptCost: cost}, nil); err == nil {
			t.Errorf("buildPasswordHasher(%d) succeeded, want rejection", cost)
		}
	}
}

func TestBuildPaymentsSelectsProvider(t *testing.T) {
	stubAdapter, err := buildPayments(config.Payments{}, "", nil)
	if err != nil || stubAdapter == nil {
		t.Fatalf("default buildPayments = %v, %v", stubAdapter, err)
	}

	if _, err := buildPayments(config.Payments{Provider: "stripe"}, "", nil); err == nil {
		t.Error("stripe without a public URL succeeded")
	}
	if _, err := buildPayments(config.Payments{Provider: "paypal"}, "", nil); err == nil {
		t.Error("unknown provider succeeded")
	}

	stripeAdapter, err := buildPayments(config.Payments{
		Provider: "stripe",
		Stripe: config.Stripe{
			SecretKey:     "sk_test_x",
			WebhookSecret: "whsec_x",
			PriceID:       "price_x",
		},
	}, "https://example.test", nil)
	if err != nil || stripeAdapter == nil {
		t.Fatalf("valid stripe buildPayments = %v, %v", stripeAdapter, err)
	}
}

func TestNewAuthCookieModeWiresServiceAndAuthStore(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth = config.Auth{
		Mode:       "cookie",
		SessionTTL: 2 * time.Hour,
		CookieName: "insteadly_session",
		BcryptCost: 4,
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	if a.Auth == nil {
		t.Fatal("Auth service is nil")
	}
	if got := a.Auth.SessionTTL(); got != 2*time.Hour {
		t.Errorf("session ttl = %v, want 2h", got)
	}
	// The local store is also the auth store: register/login must work end to end.
	user, token, err := a.Auth.Register(context.Background(), "wired@example.test", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	authenticated, err := a.Auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.ID != user.ID {
		t.Errorf("authenticated user = %q, want %q", authenticated.ID, user.ID)
	}
}

func TestNewWiresStripeAdapterWithoutNetwork(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.PublicURL = "https://example.test"
	cfg.Payments = config.Payments{
		Provider: "stripe",
		Stripe: config.Stripe{
			SecretKey:     "sk_test_x",
			WebhookSecret: "whsec_x",
			PriceID:       "price_x",
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = a.CloseStorage()
	})
	if a.Payments == nil {
		t.Fatal("stripe Payments not wired")
	}
}

func TestNewWiresTelnyxOpenAIAndNotify(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.PublicURL = "https://example.test"
	cfg.Telephony = config.Telephony{
		Provider: "telnyx",
		Telnyx: config.Telnyx{
			APIKey:       "KEY_test",
			ConnectionID: "conn_test",
			FromNumber:   "+15551234567",
		},
	}
	cfg.Dialogue = config.Dialogue{Provider: "openai"}
	cfg.OpenAI = config.OpenAI{
		APIKey:        "sk-test",
		BaseURL:       "https://api.openai.com/v1",
		DialogueModel: "gpt-4o-mini",
	}
	cfg.Notify = config.Notify{Provider: "log", Webhook: config.NotifyWebhook{Channel: "email"}}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Telephony == nil || a.Dialogue == nil || a.Notify == nil {
		t.Fatalf("telnyx/openai/notify not wired: %+v", a)
	}
	if a.Webhooks == nil {
		t.Error("telnyx webhook handler not mounted")
	}
	if a.notifyWorker == nil {
		t.Error("notify worker not built when notify.provider=log")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.Start(ctx)
	if err := a.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestNewNotifyWebhookWiresAdapter(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notify = config.Notify{
		Provider: "webhook",
		Webhook:  config.NotifyWebhook{URL: "https://hooks.example.test/insteadly", Channel: "sms"},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Notify == nil || a.notifyWorker == nil {
		t.Fatal("webhook notifier/worker not wired")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestNewRejectsNotifyWebhookWithoutURL(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notify = config.Notify{Provider: "webhook", Webhook: config.NotifyWebhook{Channel: "email"}}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected a notify webhook url error at construction")
	}
}

func TestNewNotifyOffHasNoWorker(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notify = config.Notify{Provider: "off", Webhook: config.NotifyWebhook{Channel: "email"}}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Notify != nil || a.notifyWorker != nil {
		t.Fatal("notify.provider=off must build neither a notifier nor a worker")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}
