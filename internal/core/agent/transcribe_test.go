package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/ports"
)

func transcribeAgent(t *testing.T, stt *fakeSTT, sleep func(context.Context, time.Duration) error) *Agent {
	t.Helper()
	cfg := testConfig(newFakeStore())
	cfg.STT = stt
	if sleep != nil {
		cfg.Sleep = sleep
	}
	return fastAgent(t, cfg)
}

func TestTranscribeOneStreamPerUtterance(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"Guten Tag."}, partials: []string{"Guten"}}}
	var sleeps []time.Duration
	a := transcribeAgent(t, stt, func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	})
	framesIn := frames(3, pcmFrame(2000))
	text, err := a.transcribe(context.Background(), framesIn, []string{"Ordination"})
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "Guten Tag." {
		t.Errorf("text = %q", text)
	}
	if stt.opens != 1 || stt.closes != 1 {
		t.Errorf("opens/closes = %d/%d", stt.opens, stt.closes)
	}
	if len(stt.feeds) != len(framesIn) {
		t.Fatalf("feeds = %d, want %d", len(stt.feeds), len(framesIn))
	}
	for i, frame := range framesIn {
		if !reflect.DeepEqual(stt.feeds[i], frame) {
			t.Errorf("feed %d differs", i)
		}
	}
	wantOpts := ports.StreamOptions{Language: "de-AT", Hints: []string{"Ordination"}}
	if !reflect.DeepEqual(stt.opts[0], wantOpts) {
		t.Errorf("options = %+v", stt.opts[0])
	}
	if len(sleeps) != len(framesIn)-1 {
		t.Fatalf("sleeps = %d, want %d", len(sleeps), len(framesIn)-1)
	}
	for _, d := range sleeps {
		if d != defaultReplayPace {
			t.Errorf("pace = %v, want %v", d, defaultReplayPace)
		}
	}
	if stt.closeCtx[0] != nil {
		t.Errorf("Close was called with an already-canceled context: %v", stt.closeCtx[0])
	}
}

func TestTranscribeFinalValidation(t *testing.T) {
	cases := []struct {
		name   string
		script fakeSTTScript
		want   string
		wantOK bool
	}{
		{name: "empty final", script: fakeSTTScript{finals: []string{""}}, want: "", wantOK: true},
		{name: "no final", script: fakeSTTScript{partials: []string{"half"}}},
		{name: "results error", script: fakeSTTScript{resultsErr: true}},
		{name: "multiple finals", script: fakeSTTScript{finals: []string{"a", "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stt := &fakeSTT{script: tc.script}
			a := transcribeAgent(t, stt, nil)
			text, err := a.transcribe(context.Background(), frames(2, pcmFrame(2000)), nil)
			if tc.wantOK {
				if err != nil || text != tc.want {
					t.Fatalf("text=%q err=%v", text, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got text %q", text)
			}
			if stt.closes != 1 {
				t.Errorf("Close count = %d", stt.closes)
			}
		})
	}
}

func TestTranscribeErrors(t *testing.T) {
	openErr := errors.New("open failed")
	if _, err := transcribeAgent(t, &fakeSTT{script: fakeSTTScript{openErr: openErr}}, nil).
		transcribe(context.Background(), frames(1, pcmFrame(2000)), nil); !errors.Is(err, openErr) {
		t.Fatalf("open error = %v", err)
	}

	feedErr := errors.New("feed failed")
	stt := &fakeSTT{script: fakeSTTScript{feedErr: feedErr, finals: []string{"x"}}}
	if _, err := transcribeAgent(t, stt, nil).
		transcribe(context.Background(), frames(3, pcmFrame(2000)), nil); !errors.Is(err, feedErr) {
		t.Fatalf("feed error = %v", err)
	}
	if stt.closes != 1 {
		t.Errorf("Close not called after feed error")
	}

	closeErr := errors.New("close failed")
	stt = &fakeSTT{script: fakeSTTScript{closeErr: closeErr, finals: []string{"x"}}}
	if _, err := transcribeAgent(t, stt, nil).
		transcribe(context.Background(), frames(1, pcmFrame(2000)), nil); !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
}

func TestTranscribeCancellationDuringPacingStillCloses(t *testing.T) {
	stt := &fakeSTT{script: fakeSTTScript{finals: []string{"x"}}}
	var calls int
	a := transcribeAgent(t, stt, func(context.Context, time.Duration) error {
		calls++
		if calls == 2 {
			return context.Canceled
		}
		return nil
	})
	text, err := a.transcribe(context.Background(), frames(10, pcmFrame(2000)), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if text != "" {
		t.Errorf("partial text returned on cancel: %q", text)
	}
	if stt.closes != 1 {
		t.Errorf("Close count = %d, want 1", stt.closes)
	}
	// Pacing stopped early: the second sleep returned the cancellation.
	if calls != 2 {
		t.Errorf("sleep calls = %d, want 2", calls)
	}
	if len(stt.feeds) != 2 {
		t.Errorf("feeds = %d, want 2", len(stt.feeds))
	}
}

func TestTranscribeContextCanceledBeforeOpen(t *testing.T) {
	stt := &fakeSTT{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transcribeAgent(t, stt, nil).transcribe(ctx, frames(1, pcmFrame(2000)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if stt.opens != 0 {
		t.Error("opened a stream with a canceled context")
	}
}

func TestTranscribeRaceClean(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stt := &fakeSTT{script: fakeSTTScript{finals: []string{"ok"}, partials: []string{"o"}}}
			a := transcribeAgent(t, stt, func(context.Context, time.Duration) error { return nil })
			if _, err := a.transcribe(context.Background(), frames(5, pcmFrame(2000)), nil); err != nil {
				t.Errorf("transcribe: %v", err)
			}
		}()
	}
	wg.Wait()
}
