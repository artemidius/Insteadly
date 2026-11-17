//go:build live

package app_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	dialoguemistral "github.com/artemidius/insteadly/internal/adapters/dialogue/mistral"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	sttmistral "github.com/artemidius/insteadly/internal/adapters/stt/mistral"
	ttsmistral "github.com/artemidius/insteadly/internal/adapters/tts/mistral"
	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
)

// TestLiveM0NoPhone proves the real Mistral STT → dialogue → tool continuation →
// finish_call → TTS → canonical outbound chain with fake telephony. It deliberately
// makes no phone call and reads no Twilio configuration.
//
//	LIVE_MISTRAL=1 MISTRAL_API_KEY=... make live-test-m0
//
// The scripted receptionist authorizes the capture and ends the automated test; the test
// asserts the resulting capture/tool round, not model prose quality. A wrong or missing
// tool/outcome fails for investigation — there is no retry. No key, prompt, transcript,
// patient value, or audio is logged.
func TestLiveM0NoPhone(t *testing.T) {
	if os.Getenv("LIVE_MISTRAL") != "1" {
		t.Skip("set LIVE_MISTRAL=1 to run the live M0 no-phone test")
	}
	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tts, err := ttsmistral.New(ttsmistral.Config{APIKey: key, Logger: logger})
	if err != nil {
		t.Fatalf("tts New: %v", err)
	}
	stt, err := sttmistral.New(sttmistral.Config{APIKey: key, Logger: logger})
	if err != nil {
		t.Fatalf("stt New: %v", err)
	}
	dialogue, err := dialoguemistral.New(dialoguemistral.Config{APIKey: key, Logger: logger})
	if err != nil {
		t.Fatalf("dialogue New: %v", err)
	}

	// One real TTS synthesis creates the scripted receptionist utterance. It explicitly
	// asks to capture the reason and authorizes the automated abort.
	const scriptedUtterance = "Guten Tag. Bitte erfassen Sie als Grund Kontrolle. " +
		"Das ist ein automatischer Test; bitte beenden Sie den Anruf und melden Sie ihn als abgebrochen."
	ttsCtx, ttsCancel := context.WithTimeout(ctx, 90*time.Second)
	speech, err := tts.Speak(ttsCtx, ports.SpeakOptions{})
	if err != nil {
		ttsCancel()
		t.Fatalf("tts Speak: %v", err)
	}
	if err := speech.FeedText(ttsCtx, scriptedUtterance); err != nil {
		ttsCancel()
		t.Fatalf("tts FeedText: %v", err)
	}
	var inboundFrames [][]byte
	audioDone := make(chan struct{})
	go func() {
		for chunk := range speech.Audio() {
			inboundFrames = append(inboundFrames, append([]byte(nil), chunk.Data...))
		}
		close(audioDone)
	}()
	if err := speech.Close(ttsCtx); err != nil {
		ttsCancel()
		t.Fatalf("tts Close: %v", err)
	}
	<-audioDone
	ttsCancel()
	if len(inboundFrames) == 0 {
		t.Fatal("scripted TTS produced no canonical frames")
	}
	// At least 700 ms of digital silence makes the M0 boundary deterministic.
	for i := 0; i < 35; i++ {
		inboundFrames = append(inboundFrames, make([]byte, domain.FrameBytes))
	}
	t.Logf("live no-phone: scripted utterance frames=%d", len(inboundFrames))

	storage, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage New: %v", err)
	}
	order := domain.Order{
		Status: domain.OrderScheduled,
		Clinic: domain.Clinic{Name: "Ordination Live Test", Phone: "+430000000"},
		Patient: domain.Patient{
			FullName: "Synthetic Live Testpatient",
			Reason:   "Test",
		},
	}
	created, err := storage.Orders().Create(ctx, order)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := storage.Orders().Update(ctx, created.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Transition(domain.OrderScheduled)
	}); err != nil {
		t.Fatalf("seed scheduled: %v", err)
	}

	ag, err := agent.New(agent.Config{
		STT: stt, TTS: tts, Dialogue: dialogue, Orders: storage.Orders(), Logger: logger,
	})
	if err != nil {
		t.Fatalf("agent New: %v", err)
	}
	session := newIntegrationSession()
	session.sendProgress()
	for _, frame := range inboundFrames {
		session.inbound <- domain.AudioChunk{Data: frame}
	}
	run, err := runner.New(runner.Config{
		Telephony: &integrationTelephony{session: session},
		Orders:    storage.Orders(),
		Agent:     ag,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("runner New: %v", err)
	}

	final, runErr := run.Run(ctx, created.ID)
	if runErr != nil {
		t.Fatalf("live runner: %v", runErr)
	}
	if final.Outcome == nil {
		t.Fatal("live runner returned no outcome")
	}
	t.Logf("live no-phone: outcome=%s fail_reason=%q attempts=%d", final.Outcome.Kind, final.Outcome.FailReason, final.Attempts)

	if final.Patient.Reason != "Kontrolle" {
		t.Fatal("capture_info did not persist the expected reason (model/tool round failed; investigate, do not retry)")
	}
	if final.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("outcome = %q, want aborted", final.Outcome.Kind)
	}
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", final.Attempts)
	}

	session.mu.Lock()
	sent := append([][]byte(nil), session.sentAudio...)
	hangups := session.hangupCount
	session.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("no real outbound canonical audio")
	}
	nonZero := false
	for _, frame := range sent {
		if len(frame) != domain.FrameBytes {
			t.Fatalf("outbound frame length = %d, want %d", len(frame), domain.FrameBytes)
		}
		for _, b := range frame {
			if b != 0 {
				nonZero = true
				break
			}
		}
	}
	if !nonZero {
		t.Error("outbound canonical audio was silent")
	}
	if hangups != 1 {
		t.Errorf("hangups = %d, want 1", hangups)
	}
	t.Logf("live no-phone: outbound frames=%d hangups=%d", len(sent), hangups)

	if bytes.Contains(logs.Bytes(), []byte(key)) {
		t.Error("API key leaked into logs")
	}
}
