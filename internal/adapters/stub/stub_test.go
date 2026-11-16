package stub

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func nextEvent(t *testing.T, ch <-chan domain.CallEvent) (domain.CallEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for call event")
		return domain.CallEvent{}, false
	}
}

func testOrder() domain.Order {
	return domain.Order{ID: domain.NewOrderID(), Clinic: domain.Clinic{Name: "Praxis", Phone: "+431234567"}}
}

func TestTelephonyEventSequence(t *testing.T) {
	tp := NewTelephony(nil)
	sess, err := tp.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if sess.ID() == "" {
		t.Error("session ID is empty")
	}

	for _, want := range []domain.CallEventKind{domain.EventDialing, domain.EventRinging, domain.EventAnswered} {
		ev, ok := nextEvent(t, sess.Events())
		if !ok {
			t.Fatalf("events closed before %q", want)
		}
		if ev.Kind != want {
			t.Fatalf("event = %q, want %q", ev.Kind, want)
		}
		if ev.CallID != sess.ID() {
			t.Errorf("event call id = %q, want %q", ev.CallID, sess.ID())
		}
	}

	select {
	case chunk := <-sess.Inbound():
		if len(chunk.Data) != domain.FrameBytes {
			t.Errorf("inbound chunk = %d bytes, want %d", len(chunk.Data), domain.FrameBytes)
		}
		for i, b := range chunk.Data {
			if b != 0 {
				t.Fatalf("inbound byte %d = %d, want silence", i, b)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound audio")
	}

	if err := sess.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	ev, ok := nextEvent(t, sess.Events())
	if !ok {
		t.Fatal("events closed before ended")
	}
	if ev.Kind != domain.EventEnded || ev.EndReason != domain.EndCompleted {
		t.Errorf("final event = %+v, want ended/completed", ev)
	}
	if _, ok := <-sess.Events(); ok {
		t.Error("events channel not closed after ended")
	}
	if _, ok := <-sess.Inbound(); ok {
		t.Error("inbound channel not closed after ended")
	}
}

func TestTelephonyHangupIdempotentAndConcurrent(t *testing.T) {
	tp := NewTelephony(nil)
	sess, err := tp.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sess.Hangup(context.Background()); err != nil {
				t.Errorf("Hangup: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := sess.Hangup(context.Background()); err != nil {
		t.Fatalf("third Hangup: %v", err)
	}

	ended := 0
	for {
		ev, ok := nextEvent(t, sess.Events())
		if !ok {
			break
		}
		if ev.Kind == domain.EventEnded {
			ended++
		}
	}
	if ended != 1 {
		t.Errorf("saw %d ended events, want exactly 1", ended)
	}
}

func TestTelephonyRequiresPhone(t *testing.T) {
	tp := NewTelephony(nil)
	if _, err := tp.Dial(context.Background(), domain.Order{}); err == nil {
		t.Fatal("Dial without clinic phone should fail")
	}
}

func TestTelephonySendAudioAndDTMF(t *testing.T) {
	tp := NewTelephony(nil)
	sess, err := tp.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := sess.SendAudio(context.Background(), domain.AudioChunk{Data: make([]byte, 4)}); err != nil {
		t.Errorf("SendAudio: %v", err)
	}
	if err := sess.SendDTMF(context.Background(), "1"); err != nil {
		t.Errorf("SendDTMF: %v", err)
	}
	if err := sess.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if err := sess.SendAudio(context.Background(), domain.AudioChunk{}); err != nil {
		t.Errorf("SendAudio after hangup: %v", err)
	}
	if err := sess.SendDTMF(context.Background(), "2"); err != nil {
		t.Errorf("SendDTMF after hangup: %v", err)
	}
}

func TestSTTStream(t *testing.T) {
	stt := NewSTT()
	ctx := context.Background()
	stream, err := stt.TranscribeStream(ctx, ports.StreamOptions{})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	for range 3 {
		if err := stream.Feed(ctx, domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if got := len(stream.Results()); got != 0 {
		t.Errorf("results has %d buffered events before Close, want 0", got)
	}
	if err := stream.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ev, ok := <-stream.Results()
	if !ok {
		t.Fatal("results closed without a final event")
	}
	if ev.Text != "stub heard 3 chunk(s)" || !ev.Final || ev.At.IsZero() {
		t.Errorf("final event = %+v", ev)
	}
	if _, ok := <-stream.Results(); ok {
		t.Error("results channel not closed after final event")
	}
	if err := stream.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSTTFile(t *testing.T) {
	stt := NewSTT()
	got, err := stt.TranscribeFile(context.Background(), bytes.NewReader([]byte("audio")), ports.BatchOptions{})
	if err != nil {
		t.Fatalf("TranscribeFile: %v", err)
	}
	if got.Text != "stub file transcript" {
		t.Errorf("transcript = %q", got.Text)
	}
}

func TestTTSOneChunkPerFeed(t *testing.T) {
	tts := NewTTS()
	ctx := context.Background()
	stream, err := tts.Speak(ctx, ports.SpeakOptions{})
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	for _, text := range []string{"Guten ", "Tag, ", "Praxis"} {
		if err := stream.FeedText(ctx, text); err != nil {
			t.Fatalf("FeedText: %v", err)
		}
	}
	if err := stream.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	count := 0
	for chunk := range stream.Audio() {
		count++
		if len(chunk.Data) != domain.FrameBytes {
			t.Errorf("chunk %d = %d bytes, want %d", count, len(chunk.Data), domain.FrameBytes)
		}
	}
	if count != 3 {
		t.Errorf("got %d chunks, want 3", count)
	}
	if err := stream.FeedText(ctx, "late"); err == nil {
		t.Error("FeedText after Close should fail")
	}
	if err := stream.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDialogueEcho(t *testing.T) {
	d := NewDialogue()
	ctx := context.Background()
	sess, err := d.NewSession(ctx, ports.SessionRequest{OrderID: domain.NewOrderID()})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	turn, err := sess.SendUserTurn(ctx, "hello clinic")
	if err != nil {
		t.Fatalf("SendUserTurn: %v", err)
	}
	var words []string
	for chunk := range turn.TextChunks() {
		words = append(words, chunk)
	}
	if got := strings.Join(words, " "); got != "stub: hello clinic" {
		t.Errorf("echo = %q", got)
	}
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(res.ToolCalls) != 0 || res.Outcome != nil {
		t.Errorf("result = %+v, want empty", res)
	}

	turn, err = sess.SubmitToolResults(ctx, []ports.ToolResult{{ID: "1"}, {ID: "2"}})
	if err != nil {
		t.Fatalf("SubmitToolResults: %v", err)
	}
	words = nil
	for chunk := range turn.TextChunks() {
		words = append(words, chunk)
	}
	if got := strings.Join(words, " "); got != "stub: 2 tool result(s)" {
		t.Errorf("tool-result echo = %q", got)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
