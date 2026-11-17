package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

type conversation struct {
	agent *Agent
	call  *fakeCall
	ended chan domain.CallEvent
	dlg   *fakeDialogue
	tts   *fakeTTS
	done  <-chan struct {
		outcome domain.Outcome
		err     error
	}
}

func ttsSpoke(tts *fakeTTS, substr string) bool {
	tts.mu.Lock()
	defer tts.mu.Unlock()
	for _, text := range tts.texts {
		if strings.Contains(text, substr) {
			return true
		}
	}
	return false
}

func startConversation(t *testing.T, cfg Config) *conversation {
	t.Helper()
	a := fastAgent(t, cfg)
	call := newFakeCall()
	ended := make(chan domain.CallEvent)
	done := startAgent(t, a, testOrder(), call, ended)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	return &conversation{agent: a, call: call, ended: ended, dlg: cfg.Dialogue.(*fakeDialogue), tts: cfg.TTS.(*fakeTTS), done: done}
}

func TestBargeWhileModelStreamsCancelsAndReconciles(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Einen Moment"}, waitCtx: never, waitErr: context.Canceled},
		{chunks: []string{"Neue Antwort."}},
	}}
	tts := &fakeTTS{}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"erste Frage"}}, {finals: []string{"Stopp mal"}}}}
	cfg.Dialogue, cfg.TTS = dlg, tts
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "first assistant text spoken", func() bool { return ttsSpoke(tts, "Einen Moment") })
	// Speech during the still-open model turn publishes a barge before any handshake.
	c.call.push(utteranceAudio()...)
	waitFor(t, "second user turn", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 2
	})
	dlg.mu.Lock()
	interrupts := append([]string(nil), dlg.interrupts...)
	texts := append([]string(nil), dlg.texts...)
	dlg.mu.Unlock()
	if len(interrupts) != 1 {
		t.Fatalf("interrupts = %q, want one", interrupts)
	}
	if !strings.Contains(interrupts[0], "Einen Moment") {
		t.Errorf("confirmed text = %q", interrupts[0])
	}
	if texts[1] != "Stopp mal" {
		t.Errorf("barging utterance = %q", texts[1])
	}
	if c.call.clearCount() != 1 {
		t.Errorf("clear count = %d, want exactly one", c.call.clearCount())
	}
	close(c.ended)
	if _, err := resultOf(t, c.done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v, want ErrCallEnded", err)
	}
}

func TestBargeOmitsCurrentUnmarkedSegment(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Erster Satz. Zweiter Satz."}, waitCtx: never, waitErr: context.Canceled},
		{chunks: []string{"ok"}},
	}}
	tts := &fakeTTS{}
	tts.feedHook = func(text string) {
		if strings.HasPrefix(text, "Zweiter") {
			// Keep the second assistant segment in synthesis long enough that the barge
			// (published after ~100 ms of speech) always wins the race. The aborted stream
			// must not be marked even after FeedText returns.
			time.Sleep(400 * time.Millisecond)
		}
	}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"frage"}}, {finals: []string{"warte"}}}}
	cfg.Dialogue, cfg.TTS = dlg, tts
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "first segment marked", func() bool { return len(c.call.markIDs()) >= 3 })
	c.call.push(utteranceAudio()...)
	waitFor(t, "interrupt", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.interrupts) == 1
	})
	dlg.mu.Lock()
	confirmed := dlg.interrupts[0]
	dlg.mu.Unlock()
	if confirmed != "Erster Satz." {
		t.Fatalf("confirmed text = %q, want only the marked segment", confirmed)
	}
	close(c.ended)
	if _, err := resultOf(t, c.done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v", err)
	}
}

func TestBargeRequiresStrongerOnsetAndRejectsTones(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Antwort"}, waitCtx: never, waitErr: context.Canceled},
	}}
	tts := &fakeTTS{}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg.Dialogue, cfg.TTS = dlg, tts
	c := startConversation(t, cfg)
	c.call.push(utteranceAudio()...)
	waitFor(t, "assistant speaks", func() bool { return ttsSpoke(tts, "Antwort") })
	// Three onset frames (60 ms) are below the 100 ms barge threshold.
	c.call.push(speechFrames(3)...)
	time.Sleep(50 * time.Millisecond)
	dlg.mu.Lock()
	interrupts := len(dlg.interrupts)
	dlg.mu.Unlock()
	if interrupts != 0 {
		t.Fatalf("60 ms of speech interrupted the assistant")
	}
	// A strong hold tone never barges, even for 200 ms.
	c.call.push(frames(10, toneFrame(440, 12000))...)
	time.Sleep(50 * time.Millisecond)
	dlg.mu.Lock()
	interrupts = len(dlg.interrupts)
	dlg.mu.Unlock()
	if interrupts != 0 {
		t.Fatalf("a hold tone interrupted the assistant")
	}
	close(c.ended)
	_, _ = resultOf(t, c.done)
}

func TestBargeClearFailureIsOperational(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Antwort"}, waitCtx: never, waitErr: context.Canceled},
	}}
	tts := &fakeTTS{}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{scripts: []fakeSTTScript{{finals: []string{"frage"}}, {finals: []string{"stopp"}}}}
	cfg.Dialogue, cfg.TTS = dlg, tts
	a := fastAgent(t, cfg)
	call := newFakeCall()
	clearErr := errors.New("clear transport down")
	call.mu.Lock()
	call.clearErr = clearErr
	call.mu.Unlock()
	done := startAgent(t, a, testOrder(), call, nil)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	waitFor(t, "assistant speaks", func() bool { return ttsSpoke(tts, "Antwort") })
	call.push(utteranceAudio()...)
	_, err := resultOf(t, done)
	if !errors.Is(err, clearErr) {
		t.Fatalf("err = %v, want the clear failure", err)
	}
	if errors.Is(err, ErrCallEnded) {
		t.Fatal("clear failure was disguised as call termination")
	}
}

func TestBargeTerminalRaceWins(t *testing.T) {
	store := newFakeStore(testOrder())
	never := make(chan struct{})
	dlg := &fakeDialogue{scripts: []turnScript{
		{chunks: []string{"Antwort"}, waitCtx: never, waitErr: context.Canceled},
	}}
	cfg := testConfig(store)
	cfg.STT = &fakeSTT{script: fakeSTTScript{finals: []string{"frage"}}}
	cfg.Dialogue = dlg
	a := fastAgent(t, cfg)
	call := newFakeCall()
	ended := make(chan domain.CallEvent)
	done := startAgent(t, a, testOrder(), call, ended)
	waitFor(t, "greeting confirmed", func() bool { return len(call.markIDs()) >= 2 })
	call.push(utteranceAudio()...)
	waitFor(t, "turn active", func() bool {
		dlg.mu.Lock()
		defer dlg.mu.Unlock()
		return len(dlg.texts) == 1
	})
	close(ended)
	if _, err := resultOf(t, done); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("err = %v, want ErrCallEnded", err)
	}
}
