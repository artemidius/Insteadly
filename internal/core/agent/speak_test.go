package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func speakAgent(t *testing.T, tts *fakeTTS) *Agent {
	t.Helper()
	cfg := testConfig(newFakeStore())
	cfg.TTS = tts
	return fastAgent(t, cfg)
}

func textStream(chunks ...string) <-chan string {
	ch := make(chan string, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch
}

func TestSpeakLazyOpenOnlyOnText(t *testing.T) {
	tts := &fakeTTS{}
	call := newFakeCall()
	spoke, err := speakAgent(t, tts).speak(context.Background(), call, textStream())
	if err != nil || spoke {
		t.Fatalf("spoke=%v err=%v", spoke, err)
	}
	if tts.opens != 0 {
		t.Errorf("TTS opened for an empty generation: %d", tts.opens)
	}
	if call.sentCount() != 0 {
		t.Errorf("sent audio for an empty generation")
	}
}

func TestSpeakForwardsTextVerbatimAndAudioInOrder(t *testing.T) {
	tts := &fakeTTS{}
	call := newFakeCall()
	texts := []string{"", "Guten ", "Tag,", " wie geht es?"}
	spoke, err := speakAgent(t, tts).speak(context.Background(), call, textStream(texts...))
	if err != nil {
		t.Fatalf("speak: %v", err)
	}
	if !spoke {
		t.Fatal("spoke = false")
	}
	if tts.opens != 1 || tts.closes != 1 {
		t.Errorf("opens/closes = %d/%d", tts.opens, tts.closes)
	}
	want := []string{"Guten ", "Tag,", " wie geht es?"}
	if len(tts.texts) != len(want) {
		t.Fatalf("texts = %q", tts.texts)
	}
	for i := range want {
		if tts.texts[i] != want[i] {
			t.Errorf("text[%d] = %q, want %q", i, tts.texts[i], want[i])
		}
	}
	if call.sentCount() != len(want) {
		t.Errorf("sent chunks = %d, want %d", call.sentCount(), len(want))
	}
	for i, chunk := range call.sent {
		if len(chunk) != domain.FrameBytes {
			t.Errorf("chunk %d length = %d", i, len(chunk))
		}
	}
}

func TestSpeakOfflineAudioArrivesOnClose(t *testing.T) {
	tts := &fakeTTS{script: fakeTTSScript{offline: true}}
	call := newFakeCall()
	spoke, err := speakAgent(t, tts).speak(context.Background(), call, textStream("Hallo"))
	if err != nil || !spoke {
		t.Fatalf("spoke=%v err=%v", spoke, err)
	}
	if call.sentCount() != 1 {
		t.Errorf("offline audio chunks = %d, want 1 (drainer active during Close)", call.sentCount())
	}
}

func TestSpeakSendAudioError(t *testing.T) {
	tts := &fakeTTS{}
	call := newFakeCall()
	sentinel := errors.New("playback failed")
	call.sendErr = sentinel
	_, err := speakAgent(t, tts).speak(context.Background(), call, textStream("a", "b", "c"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want SendAudio error", err)
	}
	if tts.closes != 1 {
		t.Errorf("TTS Close count = %d", tts.closes)
	}
}

func TestSpeakErrors(t *testing.T) {
	openErr := errors.New("open failed")
	if _, err := speakAgent(t, &fakeTTS{script: fakeTTSScript{openErr: openErr}}).
		speak(context.Background(), newFakeCall(), textStream("x")); !errors.Is(err, openErr) {
		t.Fatalf("open error = %v", err)
	}

	feedErr := errors.New("feed failed")
	tts := &fakeTTS{script: fakeTTSScript{feedErr: feedErr}}
	if _, err := speakAgent(t, tts).
		speak(context.Background(), newFakeCall(), textStream("x", "y")); !errors.Is(err, feedErr) {
		t.Fatalf("feed error = %v", err)
	}
	if len(tts.texts) != 1 {
		t.Errorf("text was replayed after feed error: %q", tts.texts)
	}

	closeErr := errors.New("close failed")
	if _, err := speakAgent(t, &fakeTTS{script: fakeTTSScript{closeErr: closeErr}}).
		speak(context.Background(), newFakeCall(), textStream("x")); !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
}

func TestSpeakContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := speakAgent(t, &fakeTTS{}).speak(ctx, newFakeCall(), textStream("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSpeakCancellationClosesOpenStream(t *testing.T) {
	tts := &fakeTTS{}
	opened := make(chan struct{})
	tts.feedHook = func(string) { close(opened) }
	blocked := make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := speakAgent(t, tts).speak(ctx, newFakeCall(), blocked)
		done <- err
	}()
	blocked <- "first"
	<-opened
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if tts.closes != 1 {
		t.Errorf("TTS Close count = %d, want 1", tts.closes)
	}
}
