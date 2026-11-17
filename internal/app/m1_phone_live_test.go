//go:build live

package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
)

// TestLiveM1Phone is the separately gated real-phone M1 evidence harness. It must never ring
// without every explicit gate and a controlled destination. DESIGN real-phone M1 additionally
// requires manual observation of audible stream audio, natural end-of-turn, an interrupted
// long answer that actually stops, a controlled IVR advancing only after the expected digit,
// controlled hold music not transcribed as a user, and exactly one terminal outcome.
//
//	LIVE_M1_PHONE=1 LIVE_TWILIO=1 LIVE_MISTRAL=1 MISTRAL_API_KEY=... \
//	TEST_PHONE=+43... [LIVE_M1_IVR=1 TEST_IVR_PHONE=+43...] make live-test-m1-phone
//
// The Twilio trial currently strips <Stream>; the harness therefore fails explicitly rather
// than pretending a real call succeeded. This test is never part of `make verify` or CI.
func TestLiveM1Phone(t *testing.T) {
	if os.Getenv("LIVE_M1_PHONE") != "1" {
		t.Skip("set LIVE_M1_PHONE=1 to arm the real-phone M1 harness")
	}
	if os.Getenv("LIVE_TWILIO") != "1" {
		t.Skip("set LIVE_TWILIO=1 to allow a real Twilio call")
	}
	if os.Getenv("LIVE_MISTRAL") != "1" {
		t.Skip("set LIVE_MISTRAL=1 to allow real Mistral calls")
	}
	if os.Getenv("MISTRAL_API_KEY") == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}
	to := os.Getenv("TEST_PHONE")
	if to == "" {
		t.Skip("TEST_PHONE is not set (controlled destination required)")
	}
	if os.Getenv("LIVE_M1_IVR") == "1" && os.Getenv("TEST_IVR_PHONE") == "" {
		t.Skip("LIVE_M1_IVR=1 requires TEST_IVR_PHONE (a controlled test IVR, never a clinic)")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Telephony.Provider != "twilio" {
		t.Fatal("INSTEADLY_TELEPHONY_PROVIDER must be twilio")
	}
	if cfg.STT.Provider != "mistral" || cfg.TTS.Provider != "mistral" || cfg.Dialogue.Provider != "mistral" {
		t.Fatal("all three AI providers must be mistral for the M1 phone exit")
	}
	if cfg.HTTP.PublicURL == "" {
		t.Fatal("INSTEADLY_HTTP_PUBLIC_URL must be a public tunnel URL for real Media Streams")
	}

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Close(ctx)
	})

	order := domain.Order{
		Status: domain.OrderScheduled,
		Clinic: domain.Clinic{Name: "Controlled M1 Test", Phone: to},
		Patient: domain.Patient{
			FullName: "Synthetic M1 Testpatient",
			Reason:   "Test",
			PreferredSlots: []domain.TimeRange{{
				Start: time.Now().Add(72 * time.Hour),
				End:   time.Now().Add(80 * time.Hour),
			}},
		},
	}
	created, err := a.Storage.Orders().Create(context.Background(), order)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := a.Storage.Orders().Update(context.Background(), created.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Transition(domain.OrderScheduled)
	}); err != nil {
		t.Fatalf("seed scheduled: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	final, runErr := a.Runner.Run(ctx, created.ID)
	if runErr != nil {
		t.Fatalf("live M1 phone runner: %v (record the safe observed failure; do not claim real M1)", runErr)
	}
	if final.Outcome == nil {
		t.Fatal("real-phone M1 returned no structured outcome")
	}
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly one", final.Attempts)
	}
	t.Logf("live M1 phone: outcome=%s fail_reason=%q attempts=%d", final.Outcome.Kind, final.Outcome.FailReason, final.Attempts)
	t.Log("manual evidence still required: audible two-way stream, natural end-of-turn, stopped barge playback, controlled IVR advancement, controlled hold not transcribed")
}
