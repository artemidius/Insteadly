package mistral

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/coder/websocket"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func TestRealtimeHandshakeAndVoice(t *testing.T) {
	a, c := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{Voice: "voice_override"})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	t.Cleanup(func() {
		go func() {
			for range stream.Audio() {
			}
		}()
		_ = stream.Close(context.Background())
	})

	auth, path, model, update, _, _ := c.get()
	if auth != "Bearer "+testAPIKey {
		t.Errorf("auth = %q", auth)
	}
	if path != realtimeSpeechPath {
		t.Errorf("path = %q", path)
	}
	if model != defaultModel {
		t.Errorf("model = %q", model)
	}
	sess := update["session"].(map[string]any)
	if sess["voice"] != "voice_override" {
		t.Errorf("voice = %v, want voice_override", sess["voice"])
	}
	if sess["streaming_mode"] != "responsive" {
		t.Errorf("streaming_mode = %v", sess["streaming_mode"])
	}
	if sess["response_format"] != "pcm" {
		t.Errorf("response_format = %v", sess["response_format"])
	}
}

func TestRealtimeDefaultVoice(t *testing.T) {
	a, c := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	t.Cleanup(func() {
		go func() {
			for range stream.Audio() {
			}
		}()
		_ = stream.Close(context.Background())
	})
	_, _, _, update, _, _ := c.get()
	if update["session"].(map[string]any)["voice"] != defaultVoice {
		t.Errorf("voice = %v, want default %v", update["session"].(map[string]any)["voice"], defaultVoice)
	}
}

func TestRealtimeFullFlow(t *testing.T) {
	a, c := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if err := stream.FeedText(context.Background(), "Guten "); err != nil {
		t.Fatalf("FeedText: %v", err)
	}
	if err := stream.FeedText(context.Background(), "Tag."); err != nil {
		t.Fatalf("FeedText: %v", err)
	}

	audioDone := make(chan [][]byte, 1)
	go func() {
		var frames [][]byte
		for c := range stream.Audio() {
			frames = append(frames, c.Data)
		}
		audioDone <- frames
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	frames := <-audioDone
	if len(frames) == 0 {
		t.Fatal("no canonical frames produced")
	}
	for i, f := range frames {
		if len(f) != domain.FrameBytes {
			t.Fatalf("frame %d = %d bytes, want %d", i, len(f), domain.FrameBytes)
		}
	}

	_, _, _, _, texts, end := c.get()
	if !end {
		t.Error("input_text.end not sent")
	}
	if len(texts) != 2 || texts[0] != "Guten " || texts[1] != "Tag." {
		t.Errorf("texts = %q", texts)
	}
}

func TestRealtimeSplitDeltasMatchReference(t *testing.T) {
	// Two split base64 deltas must produce the same canonical output as one conversion of
	// the combined samples.
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "x")

	var frames []byte
	done := make(chan struct{})
	go func() {
		for c := range stream.Audio() {
			frames = append(frames, c.Data...)
		}
		close(done)
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done

	audio := sineFloat32(480, 440)
	r := newResampler()
	ref := append(r.feed(toFloat64(audio)), r.finish()...)
	want := int16LE(ref)

	if len(want) != len(frames) {
		t.Fatalf("canonical bytes = %d, reference = %d", len(frames), len(want))
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Fatalf("byte %d = %d, reference %d", i, frames[i], want[i])
		}
	}
}

func toFloat64(samples []float32) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = float64(s)
	}
	return out
}

func TestRealtimeNoAutoFlush(t *testing.T) {
	// The port has no next-generation operation; the adapter must never send
	// input_text.flush on punctuation or pauses.
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "Guten Tag. ")
	go func() {
		for range stream.Audio() {
		}
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRealtimeFeedTextValidation(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	t.Cleanup(func() {
		go func() {
			for range stream.Audio() {
			}
		}()
		_ = stream.Close(context.Background())
	})

	if err := stream.FeedText(context.Background(), ""); err != nil {
		t.Errorf("empty text should be a no-op, got %v", err)
	}
	if err := stream.FeedText(context.Background(), string([]byte{0xff, 0xfe})); err == nil {
		t.Error("invalid UTF-8 must error")
	}
	big := make([]byte, maxTextDelta+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := stream.FeedText(context.Background(), string(big)); err == nil {
		t.Error("oversized delta must error")
	}
}

func TestRealtimeFeedTextAfterClose(t *testing.T) {
	a, _ := newRealtimeServer(t, defaultScript)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	go func() {
		for range stream.Audio() {
		}
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.FeedText(context.Background(), "late"); err != ErrStreamClosed {
		t.Errorf("FeedText after close = %v, want ErrStreamClosed", err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestRealtimeProviderError(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn) // first append
		wsWrite(t, conn, map[string]any{"type": "error", "code": "model_overloaded", "message": "sensitive"})
		_ = conn.CloseNow()
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()
	err = stream.Close(context.Background())
	pe, ok := err.(*ProtocolError)
	if !ok || pe.Code != "model_overloaded" {
		t.Errorf("Close = %v, want ProtocolError model_overloaded", err)
	}
}

func TestRealtimeMalformedAudioDelta(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		_ = wsReadMap(t, conn) // end
		wsWrite(t, conn, map[string]any{"type": "speech.audio.delta", "audio_data": "not-base64!!!"})
		_ = conn.CloseNow()
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()
	err = stream.Close(context.Background())
	if _, ok := err.(*ProtocolError); !ok {
		t.Errorf("Close = %v, want ProtocolError", err)
	}
}

func TestRealtimeHandshakeRejectsWrongResponseFormat(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{"response_format": "mp3"}})
		_ = conn.CloseNow()
	}
	a, _ := newRealtimeServer(t, script)
	if _, err := a.Speak(context.Background(), ports.SpeakOptions{}); err == nil {
		t.Fatal("expected handshake error for mp3 response format")
	}
}

func TestRealtimeAudioDoneMissing(t *testing.T) {
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		_ = wsReadMap(t, conn)                            // end
		_ = conn.Close(websocket.StatusNormalClosure, "") // no audio.done
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	go func() {
		for range stream.Audio() {
		}
	}()
	err = stream.Close(context.Background())
	if _, ok := err.(*ProtocolError); !ok {
		t.Errorf("Close = %v, want ProtocolError (audio.done missing)", err)
	}
}

func TestRealtimeOutputIsPCM16LE(t *testing.T) {
	// The canonical output must be PCM16 little-endian; feed a constant float32 value and
	// check the decoded samples reflect the conversion.
	script := func(t *testing.T, conn *websocket.Conn, c *ttsCapture) {
		wsWrite(t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		wsWrite(t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
		_ = wsReadMap(t, conn)
		_ = wsReadMap(t, conn) // end
		// 480 constant samples at 0.5.
		constant := make([]float32, 480)
		for i := range constant {
			constant[i] = 0.5
		}
		wsWrite(t, conn, map[string]any{"type": "speech.audio.delta", "audio_data": base64.StdEncoding.EncodeToString(float32Bytes(constant))})
		wsWrite(t, conn, map[string]any{"type": "speech.audio.done"})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	a, _ := newRealtimeServer(t, script)
	stream, err := a.Speak(context.Background(), ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	_ = stream.FeedText(context.Background(), "hi")
	var all []byte
	done := make(chan struct{})
	go func() {
		for c := range stream.Audio() {
			all = append(all, c.Data...)
		}
		close(done)
	}()
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done
	if len(all) != 640 {
		t.Fatalf("output bytes = %d, want 640", len(all))
	}
	first := int16(binary.LittleEndian.Uint16(all[:2]))
	if first <= 0 {
		t.Errorf("first sample = %d, want positive (0.5 input)", first)
	}
}

var _ ports.SpeechStream = (*realtimeStream)(nil)
