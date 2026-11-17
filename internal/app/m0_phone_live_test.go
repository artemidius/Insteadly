//go:build live

package app_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/httpapi"
)

// TestLiveM0Phone is the deferred DESIGN M0 real-phone harness. It is separately gated so
// no other live target can ever ring a phone. The current Twilio account is a trial that
// strips <Stream>; this harness must skip/fail explicitly until the account is upgraded,
// a public tunnel is running, and the user explicitly approves a real call.
//
//	LIVE_M0_PHONE=1 LIVE_TWILIO=1 LIVE_MISTRAL=1 MISTRAL_API_KEY=... \
//	TEST_PHONE=+43... make live-test-m0-phone
//
// Operator script: answer the call, wait for the complete greeting (do not test barge-in),
// speak one clear German sentence that offers and explicitly confirms a slot inside the
// wide preference window, and stay on the line until the assistant hangs up. Success
// requires an actual Media Stream attach (the runner's StreamReady gate), audible speech,
// STT/dialogue/tool progress, and a terminal booked order with exactly one attempt.
func TestLiveM0Phone(t *testing.T) {
	if os.Getenv("LIVE_M0_PHONE") != "1" {
		t.Skip("set LIVE_M0_PHONE=1 to arm the real-phone M0 harness")
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
		t.Skip("TEST_PHONE is not set")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Telephony.Provider != "twilio" {
		t.Fatal("INSTEADLY_TELEPHONY_PROVIDER must be twilio")
	}
	if cfg.STT.Provider != "mistral" || cfg.TTS.Provider != "mistral" || cfg.Dialogue.Provider != "mistral" {
		t.Fatal("all three AI providers must be mistral for the M0 phone exit")
	}
	if cfg.HTTP.PublicURL == "" {
		t.Fatal("INSTEADLY_HTTP_PUBLIC_URL (tunnel) is required")
	}

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		t.Fatalf("listen %s: %v", cfg.HTTP.Addr, err)
	}
	srv := httpapi.New(cfg, a, "live-m0-phone")
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Runner drains active calls before the HTTP surface it needs disappears.
		_ = a.Close(ctx)
		_ = srv.Shutdown(ctx)
	}()

	now := time.Now()
	order := domain.Order{
		Status: domain.OrderScheduled,
		Clinic: domain.Clinic{Name: "Ordination M0 Live Test", Phone: to},
		Patient: domain.Patient{
			FullName: "Synthetic M0 Testpatient",
			Reason:   "Kontrolle",
			PreferredSlots: []domain.TimeRange{{
				Start: now.Add(24 * time.Hour),
				End:   now.Add(7 * 24 * time.Hour),
			}},
		},
	}
	created, err := a.Storage.Orders().Create(context.Background(), order)
	if err != nil {
		t.Fatalf("Create order: %v", err)
	}
	if _, err := a.Storage.Orders().Update(context.Background(), created.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Transition(domain.OrderScheduled)
	}); err != nil {
		t.Fatalf("seed scheduled: %v", err)
	}

	// This is the only invocation surface in C3: no public place-call endpoint exists.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	final, err := a.Runner.Run(ctx, created.ID)
	if err != nil {
		// Adapter errors carry a status/stable code only, never bodies or numbers.
		t.Fatalf("real-phone M0 runner: %v", err)
	}
	if final.Outcome == nil {
		t.Fatal("real-phone M0 returned no outcome")
	}
	t.Logf("real-phone M0: outcome=%s fail_reason=%q attempts=%d",
		final.Outcome.Kind, final.Outcome.FailReason, final.Attempts)

	if final.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %q, want booked (a no-stream/trial run must not pass)", final.Outcome.Kind)
	}
	if final.Outcome.BookedSlot == nil {
		t.Fatal("booked outcome has no slot")
	}
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", final.Attempts)
	}
	if strings.TrimSpace(final.Patient.FullName) == "" {
		t.Error("patient name missing after the call")
	}
}
