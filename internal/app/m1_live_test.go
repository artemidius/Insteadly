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

// TestLiveM1NoPhone is the required C4 real-Mistral no-phone gate. It uses the real C2
// adapters and fake telephony only: streamed core VAD, one barge-in with clear/history
// reconciliation, and a valid terminal outcome. It never reads Twilio config, never reads
// .env, and never logs keys, prompts, transcripts, patient values, tool arguments, or
// audio.
//
//	LIVE_MISTRAL=1 MISTRAL_API_KEY=... make live-test-m1
func TestLiveM1NoPhone(t *testing.T) {
	if os.Getenv("LIVE_MISTRAL") != "1" {
		t.Skip("set LIVE_MISTRAL=1 to run the live M1 no-phone test")
	}
	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	// The barge is deliberately against the fixed greeting, not a model reply. The greeting
	// is a deterministic ~8.8 s assistant-audio window; a real reply can be short and quiet,
	// so an acoustic barge onset may arrive only after it finished. Measured real-TTS
	// "Stopp, Stopp! …" begins with ~12 near-silent frames; "Guten Tag. …" starts with
	// measurable speech energy. Feed this paced cue during the greeting. No transcript is
	// logged.
	bargeUtterance := synthesizeInbound(t, ctx, tts,
		"Guten Tag. Bitte beenden Sie den Anruf jetzt mit dem Ergebnis abgebrochen, denn das ist ein automatischer Test.")

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
	run, err := runner.New(runner.Config{
		Telephony: &integrationTelephony{session: session},
		Orders:    storage.Orders(),
		Agent:     ag,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("runner New: %v", err)
	}

	runDone := make(chan struct{})
	var final domain.Order
	var runErr error
	go func() {
		defer close(runDone)
		final, runErr = run.Run(ctx, created.ID)
	}()

	session.sendProgress()
	// Wait only for the first audible greeting frame (not the two greeting marks) so the
	// barge hits real assistant audio while the ~8.8 s greeting is still playing.
	awaitOutboundStart(t, session, 30*time.Second, runDone)
	feedPacedWhileRunning(t, session, bargeUtterance, runDone)
	select {
	case <-runDone:
	case <-time.After(4 * time.Minute):
		t.Fatal("live M1 no-phone run did not finish")
	}
	if runErr != nil {
		t.Fatalf("live runner: %v (investigate; do not retry silently)", runErr)
	}
	if final.Outcome == nil {
		t.Fatal("live runner returned no structured outcome")
	}
	if final.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("live outcome = %+v, want aborted", final.Outcome)
	}
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", final.Attempts)
	}
	session.mu.Lock()
	clears := session.clearCount
	sentTotal := session.sentAudioTotal
	sentNonZero := session.sentAudioNonZero
	sentBadLen := session.sentAudioBadLen
	hangups := session.hangupCount
	session.mu.Unlock()
	if clears < 1 {
		t.Fatal("no barge-in clear was observed; the barge scenario did not run")
	}
	// Use the cumulative total, not the queued slice: the barge's ClearAudio correctly
	// drops queued playback, but the greeting frames were actually sent before the clear.
	if sentTotal == 0 {
		t.Fatal("no real outbound audio")
	}
	if sentBadLen != 0 {
		t.Fatalf("%d outbound frame(s) had a non-canonical length", sentBadLen)
	}
	if !sentNonZero {
		t.Error("outbound canonical audio was silent")
	}
	if hangups != 1 {
		t.Errorf("hangups = %d, want 1", hangups)
	}
	t.Logf("live M1 no-phone: outcome=%s fail_reason=%q attempts=%d clears=%d outbound_frames=%d",
		final.Outcome.Kind, final.Outcome.FailReason, final.Attempts, clears, sentTotal)
	if bytes.Contains(logs.Bytes(), []byte(key)) {
		t.Error("API key leaked into logs")
	}
}

// synthesizeInbound uses the real TTS adapter to create canonical inbound caller audio.
func synthesizeInbound(t *testing.T, ctx context.Context, tts ports.TextToSpeech, text string) [][]byte {
	t.Helper()
	ttsCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stream, err := tts.Speak(ttsCtx, ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("scripted tts Speak: %v", err)
	}
	if err := stream.FeedText(ttsCtx, text); err != nil {
		t.Fatalf("scripted tts FeedText: %v", err)
	}
	var frames [][]byte
	audioDone := make(chan struct{})
	go func() {
		for chunk := range stream.Audio() {
			frames = append(frames, append([]byte(nil), chunk.Data...))
		}
		close(audioDone)
	}()
	if err := stream.Close(ttsCtx); err != nil {
		t.Fatalf("scripted tts Close: %v", err)
	}
	<-audioDone
	if len(frames) == 0 {
		t.Fatal("scripted TTS produced no canonical frames")
	}
	for i := 0; i < 35; i++ {
		frames = append(frames, make([]byte, domain.FrameBytes))
	}
	return frames
}

func sentFrames(session *integrationSession) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.sentAudio)
}

// awaitOutboundStart waits for the greeting's first outbound frame. It deliberately does not
// wait for the greeting marks: the barge must land inside the fixed multi-second greeting
// window, not after a model reply whose length and loudness vary.
func awaitOutboundStart(t *testing.T, session *integrationSession, timeout time.Duration, runDone <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-runDone:
			t.Fatal("call ended before the greeting produced assistant audio")
		default:
		}
		if sentFrames(session) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("greeting produced no outbound assistant audio")
}

// liveFramePace is the canonical 20 ms frame rate (50 fps). A real caller's audio reaches
// the agent at this rate, which is what the bounded STT worker queue was sized for. Feeding
// a whole synthesized utterance as a tight burst here would be a harness artifact: a real
// WebSocket handshake (0.6–0.9 s) cannot drain a ~600-frame burst through a 64-frame queue.
// The credential-free integration tests keep the instant burst; only this live path paces.
const liveFramePace = 20 * time.Millisecond

// feedPacedWhileRunning writes one canonical frame per 20 ms, stopping early if the runner
// finishes. This mirrors real telephony and prevents a harness-induced queue overflow.
func feedPacedWhileRunning(t *testing.T, session *integrationSession, frames [][]byte, runDone <-chan struct{}) {
	t.Helper()
	ticker := time.NewTicker(liveFramePace)
	defer ticker.Stop()
	for _, frame := range frames {
		select {
		case <-runDone:
			return
		case <-ticker.C:
		}
		if !session.trySendFrame(frame) {
			return
		}
	}
}
