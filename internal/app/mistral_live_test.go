//go:build live

package app_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	dialoguemistral "github.com/artemidius/insteadly/internal/adapters/dialogue/mistral"
	sttmistral "github.com/artemidius/insteadly/internal/adapters/stt/mistral"
	ttsmistral "github.com/artemidius/insteadly/internal/adapters/tts/mistral"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// TestLiveMistral runs one short, sequential, idempotent smoke against the real Mistral
// API: TTS -> canonical audio -> STT -> final transcript -> one dialogue tool call. It is
// excluded from normal builds and never runs in CI or make verify.
//
//	go test -tags live -count=1 -run '^TestLiveMistral$' -v ./internal/app
//
// Required: LIVE_MISTRAL=1 and MISTRAL_API_KEY (read directly, never via app config).
// The key, prompts, transcripts, and raw audio are never logged.
func TestLiveMistral(t *testing.T) {
	if os.Getenv("LIVE_MISTRAL") != "1" {
		t.Skip("set LIVE_MISTRAL=1 to run the live Mistral smoke test")
	}
	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- TTS ---
	tts, err := ttsmistral.New(ttsmistral.Config{APIKey: key})
	if err != nil {
		t.Fatalf("tts New: %v", err)
	}
	ttsStream, err := tts.Speak(ctx, ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("tts Speak: %v", err)
	}
	if err := ttsStream.FeedText(ctx, "Guten Tag."); err != nil {
		t.Fatalf("tts FeedText: %v", err)
	}
	audioDone := make(chan struct{})
	var frames [][]byte
	go func() {
		for c := range ttsStream.Audio() {
			frames = append(frames, c.Data)
		}
		close(audioDone)
	}()
	if err := ttsStream.Close(ctx); err != nil {
		t.Fatalf("tts Close: %v", err)
	}
	<-audioDone
	if len(frames) == 0 {
		t.Fatal("TTS produced no canonical frames")
	}
	for _, f := range frames {
		if len(f) != domain.FrameBytes {
			t.Fatalf("TTS frame length = %d, want %d", len(f), domain.FrameBytes)
		}
	}
	t.Logf("live TTS produced %d canonical frames", len(frames))

	// --- STT ---
	stt, err := sttmistral.New(sttmistral.Config{APIKey: key})
	if err != nil {
		t.Fatalf("stt New: %v", err)
	}
	sttStream, err := stt.TranscribeStream(ctx, ports.StreamOptions{Language: "de"})
	if err != nil {
		t.Fatalf("stt TranscribeStream: %v", err)
	}
	for _, f := range frames {
		if err := sttStream.Feed(ctx, domain.AudioChunk{Data: f}); err != nil {
			t.Fatalf("stt Feed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resDone := make(chan []ports.TranscriptionEvent, 1)
	go func() {
		var evs []ports.TranscriptionEvent
		for ev := range sttStream.Results() {
			evs = append(evs, ev)
		}
		resDone <- evs
	}()
	if err := sttStream.Close(ctx); err != nil {
		t.Fatalf("stt Close: %v", err)
	}
	var final string
	for _, ev := range <-resDone {
		if ev.Final {
			final = ev.Text
		}
	}
	if !strings.Contains(strings.ToLower(final), "guten") {
		t.Fatal("STT final transcript does not contain the synthesized word (check case-insensitively)")
	}

	// --- Dialogue ---
	dlg, err := dialoguemistral.New(dialoguemistral.Config{APIKey: key})
	if err != nil {
		t.Fatalf("dialogue New: %v", err)
	}
	sess, err := dlg.NewSession(ctx, ports.SessionRequest{
		OrderID:       domain.NewOrderID(),
		Tools:         []domain.Tool{captureInfoLiveTool()},
		OutcomeSchema: liveOutcomeSchema(),
	})
	if err != nil {
		t.Fatalf("dialogue NewSession: %v", err)
	}
	turn, err := sess.SendUserTurn(ctx, "Bitte erfasse den Grund Kontrolle.")
	if err != nil {
		t.Fatalf("dialogue SendUserTurn: %v", err)
	}
	for range turn.TextChunks() {
		// drain streamed text; never log it
	}
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("dialogue Wait: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "capture_info" {
		t.Fatalf("dialogue produced %d capture_info calls (model refused/wrong call)", len(res.ToolCalls))
	}
	if !json.Valid(res.ToolCalls[0].Arguments) {
		t.Fatal("dialogue capture_info arguments are not valid JSON")
	}
	// Do NOT execute the tool or submit a result.
}

func captureInfoLiveTool() domain.Tool {
	return domain.Tool{
		Name:        "capture_info",
		Description: "Capture one user-authorized item of patient information.",
		Parameters:  []byte(`{"type":"object","additionalProperties":false,"properties":{"field":{"type":"string","enum":["full_name","phone","birth_date","insurance_number","reason","note"]},"value":{"type":"string","minLength":1,"maxLength":2048}},"required":["field","value"]}`),
	}
}

func liveOutcomeSchema() []byte {
	return []byte(`{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string","enum":["booked","callback_needed","failed","aborted"]},"detail":{"type":"string","minLength":1},"booked_slot":{"type":"object","additionalProperties":false,"properties":{"date_time":{"type":"string","format":"date-time"},"doctor":{"type":"string"},"practice":{"type":"string"}},"required":["date_time"]},"callback_at":{"type":"string","format":"date-time"},"fail_reason":{"type":"string","minLength":1}},"required":["kind","detail"]}`)
}
