package agent

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func startTestListener(t *testing.T, stt *fakeSTT) (*listener, *fakeCall) {
	t.Helper()
	cfg := testConfig(newFakeStore())
	cfg.STT = stt
	a := fastAgent(t, cfg)
	call := newFakeCall()
	l := newListener(a, call, []string{"Ordination"})
	l.start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		l.stop(ctx)
	})
	return l, call
}

func waitResult(t *testing.T, l *listener) utteranceResult {
	t.Helper()
	select {
	case r := <-l.resultsChannel():
		return r
	case err := <-l.errorChannel():
		t.Fatalf("listener error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no final transcript")
	}
	return utteranceResult{}
}

func TestListenerOpensSTTAtOnsetAndStreamsLive(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Guten Tag."}, partialOnFeed: "Guten"}}
	l, call := startTestListener(t, stt)
	l.setMode(modeListening)

	inbound := flatten(frames(15, silenceFrame()), speechFrames(20), frames(35, silenceFrame()))
	call.push(inbound...)
	res := waitResult(t, l)
	if res.text != "Guten Tag." {
		t.Fatalf("text = %q", res.text)
	}
	if stt.opens != 1 || stt.closes != 1 {
		t.Fatalf("STT opens/closes = %d/%d", stt.opens, stt.closes)
	}
	// Onset is confirmed on the third voiced frame (index 17). The 15-frame pre-roll is the
	// last 15 frames before/at onset (inbound[3:18]) and is burst exactly once, then every
	// remaining live frame is forwarded in order through the final hangover frame.
	expected := append([][]byte(nil), inbound[3:18]...)
	expected = append(expected, inbound[18:]...)
	if len(stt.feeds) != len(expected) {
		t.Fatalf("feeds = %d, want %d", len(stt.feeds), len(expected))
	}
	for i, frame := range stt.feeds {
		if hex.EncodeToString(frame) != hex.EncodeToString(expected[i]) {
			t.Fatalf("feed %d differs from streaming order", i)
		}
	}
	if stt.feeds[0][0] != 0 {
		t.Error("first forwarded frame should be pre-roll silence")
	}
	// The partial arrived while audio was still streaming and never caused an extra final.
}

func TestListenerDiscardsNoiseCandidateAndKeepsListening(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"echt"}}}
	l, call := startTestListener(t, stt)
	l.setMode(modeListening)
	// Three onset frames then silence: too short to be an utterance.
	call.push(flatten(speechFrames(3), frames(35, silenceFrame()))...)
	waitFor(t, "discarded worker close", func() bool {
		stt.mu.Lock()
		defer stt.mu.Unlock()
		return stt.closes == 1
	})
	select {
	case r := <-l.resultsChannel():
		t.Fatalf("discarded candidate produced a transcript: %q", r.text)
	default:
	}
	// The same listen window continues with a real utterance.
	call.push(flatten(speechFrames(20), frames(35, silenceFrame()))...)
	res := waitResult(t, l)
	if res.text != "echt" {
		t.Fatalf("text = %q", res.text)
	}
}

func TestListenerBargeSignalBeforeHandshake(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Stopp"}}}
	l, call := startTestListener(t, stt)
	l.setMode(modeAssistant)
	// 3 frames < 100 ms barge threshold.
	call.push(speechFrames(3)...)
	time.Sleep(30 * time.Millisecond)
	select {
	case <-l.bargeChannel():
		t.Fatal("barge fired before 100 ms")
	default:
	}
	call.push(speechFrames(5)...)
	select {
	case <-l.bargeChannel():
	case <-time.After(time.Second):
		t.Fatal("barge signal missing")
	}
	call.push(flatten(speechFrames(12), frames(35, silenceFrame()))...)
	res := waitResult(t, l)
	if res.text != "Stopp" {
		t.Fatalf("text = %q", res.text)
	}
}

func TestListenerPendingFinalOverflowIsAnError(t *testing.T) {
	stt := &fakeSTT{scripts: []fakeSTTScript{
		{finals: []string{"eins"}},
		{finals: []string{"zwei"}},
	}}
	l, call := startTestListener(t, stt)
	l.setMode(modeListening)
	// Two complete utterances without consuming the first result fill the one-slot pending
	// final queue; the second must fail the attempt, never be silently dropped.
	call.push(flatten(speechFrames(20), frames(35, silenceFrame()), speechFrames(20), frames(35, silenceFrame()))...)
	select {
	case err := <-l.errorChannel():
		if err == nil {
			t.Fatal("nil overflow error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not report pending-final overflow")
	}
}

func TestListenerMalformedAudioIsAProviderContractError(t *testing.T) {
	l, call := startTestListener(t, &fakeSTT{})
	l.setMode(modeListening)
	call.push([]byte{1, 2, 3})
	select {
	case err := <-l.errorChannel():
		if err == nil {
			t.Fatal("nil error for odd inbound chunk")
		}
	case <-time.After(time.Second):
		t.Fatal("malformed chunk not rejected")
	}
}

func TestListenerStopJoinsWorkers(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	cfg := testConfig(newFakeStore())
	cfg.STT = stt
	a := fastAgent(t, cfg)
	call := newFakeCall()
	l := newListener(a, call, nil)
	l.start()
	l.setMode(modeListening)
	call.push(speechFrames(5)...)
	waitFor(t, "worker open", func() bool {
		stt.mu.Lock()
		defer stt.mu.Unlock()
		return stt.opens == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	l.stop(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("stop did not join the worker promptly")
	}
	_ = domain.FrameBytes
}
