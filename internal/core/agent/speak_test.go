package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestSegmenterSentenceAndBoundaryRules(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		flush  string
		want   []string
	}{
		{name: "sentences", deltas: []string{"Hallo. ", "Wie geht es? ", "Gut!"}, want: []string{"Hallo.", "Wie geht es?", "Gut!"}},
		{name: "newline", deltas: []string{"Erste Zeile\n", "Zweite"}, want: []string{"Erste Zeile", "Zweite"}},
		{name: "empty", deltas: nil, want: nil},
		{name: "spaces only", deltas: []string{"   "}, want: nil},
		{name: "split sentence", deltas: []string{"Guten ", "Tag."}, want: []string{"Guten Tag."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s segmenter
			var got []string
			for _, d := range tc.deltas {
				got = append(got, s.push(d)...)
			}
			if tail := s.flush(); tail != "" {
				got = append(got, tail)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("segments = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("segment %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSegmenterBoundedAt240Runes(t *testing.T) {
	// A long clause with a comma near the bound splits at the comma.
	long := strings.Repeat("ä", 150) + ", " + strings.Repeat("b", 200)
	var s segmenter
	segments := append(s.push(long), s.flush())
	var all []string
	for _, seg := range segments {
		if seg != "" {
			all = append(all, seg)
		}
	}
	if len(all) < 2 {
		t.Fatalf("long text was not split: %d segment(s)", len(all))
	}
	for _, seg := range all {
		if n := utf8.RuneCountInString(seg); n > maxSegmentRunes {
			t.Errorf("segment has %d runes, exceeds %d", n, maxSegmentRunes)
		}
	}
	if !strings.HasSuffix(all[0], ", ") && !strings.HasSuffix(all[0], ",") {
		t.Logf("soft split landed at %q", all[0][len(all[0])-5:])
	}
}

func playerFixture(t *testing.T, cfg Config) (*Agent, *segmentPlayer, *fakeCall, *fakeTTS, *controlRouter) {
	t.Helper()
	tts := &fakeTTS{}
	cfg.TTS = tts
	if cfg.PlaybackAckTimeout == 0 {
		cfg.PlaybackAckTimeout = time.Second
	}
	a := fastAgent(t, cfg)
	call := newFakeCall()
	ctrl := newControlRouter(call.Control())
	ctrl.start()
	t.Cleanup(ctrl.stopRouter)
	pb := &playbackState{}
	return a, newSegmentPlayer(a, call, pb, ctrl), call, tts, ctrl
}

func TestPlayerConfirmsOnlyMarkedSegment(t *testing.T) {
	_, p, call, tts, _ := playerFixture(t, testConfig(newFakeStore()))
	// A late mark from an older segment is ignored, not treated as confirmation.
	call.controls <- domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: "old"}
	if err := p.play(context.Background(), "Einen Moment bitte.", "m-current"); err != nil {
		t.Fatalf("play: %v", err)
	}
	if got := p.confirmedText(); got != "Einen Moment bitte." {
		t.Fatalf("confirmed = %q", got)
	}
	if !p.anySpoke() {
		t.Error("spoke = false")
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if tts.opens != 1 || tts.closes != 1 {
		t.Errorf("TTS opens/closes = %d/%d", tts.opens, tts.closes)
	}
	if len(call.sent) == 0 {
		t.Error("no audio sent")
	}
}

func TestPlayerEmptySegmentOpensNoTTS(t *testing.T) {
	_, p, _, tts, _ := playerFixture(t, testConfig(newFakeStore()))
	if err := p.play(context.Background(), "   ", "m1"); err != nil {
		t.Fatalf("play: %v", err)
	}
	if tts.opens != 0 {
		t.Errorf("TTS opened for empty segment: %d", tts.opens)
	}
}

func TestPlayerAckTimeoutIsNotPlaybackEvidence(t *testing.T) {
	cfg := testConfig(newFakeStore())
	cfg.PlaybackAckTimeout = 20 * time.Millisecond
	_, p, call, _, _ := playerFixture(t, cfg)
	call.mu.Lock()
	call.markSilent = true
	call.mu.Unlock()
	err := p.play(context.Background(), "Gesprochen, aber nicht bestätigt.", "m1")
	if !errors.Is(err, errPlaybackAckTimeout) {
		t.Fatalf("err = %v, want errPlaybackAckTimeout", err)
	}
	if p.confirmedText() != "" {
		t.Fatalf("unmarked segment was recorded as confirmed: %q", p.confirmedText())
	}
}

func TestPlayerCancellationDoesNotMarkAndLateAudioIsDiscarded(t *testing.T) {
	_, p, call, tts, _ := playerFixture(t, testConfig(newFakeStore()))
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tts.feedHook = func(string) {
		once.Do(func() {
			close(blocked)
			<-release
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.play(ctx, "Ein langer Satz.", "m1") }()
	<-blocked
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("play did not return after cancellation")
	}
	if p.confirmedText() != "" {
		t.Errorf("canceled segment confirmed: %q", p.confirmedText())
	}
	call.mu.Lock()
	marks := append([]string(nil), call.marks...)
	call.mu.Unlock()
	if len(marks) != 0 {
		t.Errorf("canceled playback asked for a mark: %q", marks)
	}
}

func TestSpeakerEmptyChannelIsLazy(t *testing.T) {
	a, p, _, tts, ctrl := playerFixture(t, testConfig(newFakeStore()))
	_ = p
	texts := make(chan string)
	close(texts)
	sp := a.startSpeaker(context.Background(), newFakeCall(), &playbackState{}, ctrl, texts)
	out := <-sp.done
	if out.err != nil || out.spoke {
		t.Fatalf("out = %+v", out)
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if tts.opens != 0 {
		t.Errorf("TTS opened for tool-only turn: %d", tts.opens)
	}
}

func TestSpeakerStreamsEachSegmentWithItsOwnMark(t *testing.T) {
	a, _, call, tts, ctrl := playerFixture(t, testConfig(newFakeStore()))
	texts := make(chan string, 4)
	texts <- "Erster Satz. Zweiter Satz."
	close(texts)
	sp := a.startSpeaker(context.Background(), call, &playbackState{}, ctrl, texts)
	out := <-sp.done
	if out.err != nil {
		t.Fatalf("speaker: %v", out.err)
	}
	if len(out.confirmed) != 2 {
		t.Fatalf("confirmed = %q", out.confirmed)
	}
	if out.confirmed[0] != "Erster Satz." || out.confirmed[1] != "Zweiter Satz." {
		t.Errorf("confirmed = %q", out.confirmed)
	}
	tts.mu.Lock()
	opens := tts.opens
	tts.mu.Unlock()
	if opens != 2 {
		t.Errorf("TTS generations = %d, want one per segment", opens)
	}
	call.mu.Lock()
	marks := append([]string(nil), call.marks...)
	call.mu.Unlock()
	if len(marks) != 2 || marks[0] == marks[1] {
		t.Errorf("marks = %q, want two unique IDs", marks)
	}
}
