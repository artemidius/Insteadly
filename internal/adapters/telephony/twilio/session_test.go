package twilio

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func dialTestSession(t *testing.T) (*Adapter, *session, *fakeAPI) {
	t.Helper()
	f, ts := newFakeAPI(t)
	a := newTestAdapter(t, ts.URL)
	sess, err := a.Dial(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s := sess.(*session)
	t.Cleanup(func() {
		_ = s.Hangup(context.Background())
		drainEvents(s)
	})
	return a, s, f
}

func TestEventSequenceCompleted(t *testing.T) {
	_, s, _ := dialTestSession(t)
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	s.handleStatus("ringing")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")

	s.handleStatus("in-progress")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	s.handleStatus("completed")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndCompleted)

	if _, ok := <-s.Events(); ok {
		t.Fatal("events channel not closed after ended")
	}
	if _, ok := <-s.Inbound(); ok {
		t.Fatal("inbound channel not closed after ended")
	}
}

func TestOutOfOrderAndDuplicateCallbacks(t *testing.T) {
	_, s, _ := dialTestSession(t)
	wantEvent(t, nextEvent(t, s.Events()), domain.EventDialing, "")

	s.handleStatus("in-progress") // arrives before ringing
	wantEvent(t, nextEvent(t, s.Events()), domain.EventRinging, "")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventAnswered, "")

	s.handleStatus("ringing") // stale
	select {
	case ev := <-s.Events():
		t.Fatalf("stale ringing produced %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	s.handleStatus("busy")
	wantEvent(t, nextEvent(t, s.Events()), domain.EventEnded, domain.EndBusy)
	s.handleStatus("completed") // after ended: ignored
	if _, ok := <-s.Events(); ok {
		t.Fatal("late callback produced an extra event")
	}
}

func TestDroppedInboundIsOldest(t *testing.T) {
	_, s, _ := dialTestSession(t)
	for i := 0; i < 101; i++ {
		s.pushInbound(domain.AudioChunk{Data: []byte{byte(i)}})
	}
	got := <-s.Inbound()
	if len(got.Data) != 1 || got.Data[0] != 1 {
		t.Fatalf("first surviving chunk = %v, want chunk 1 (chunk 0 dropped)", got.Data)
	}
}

func TestConcurrentSendAudioAndHangup(t *testing.T) {
	_, s, _ := dialTestSession(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.SendAudio(context.Background(), domain.AudioChunk{Data: make([]byte, domain.FrameBytes)})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Hangup(context.Background())
	}()
	wg.Wait()

	if err := s.SendAudio(context.Background(), domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("SendAudio after hangup = %v, want ErrSessionEnded", err)
	}
}

func TestSendDTMFRejectsBadInput(t *testing.T) {
	_, s, _ := dialTestSession(t)
	s.progress(domain.EventAnswered)
	cases := []string{"", "12a", "123456789012345678901234567890123", "abc"}
	for _, digits := range cases {
		if err := s.SendDTMF(context.Background(), digits); err == nil {
			t.Errorf("SendDTMF(%q) accepted", digits)
		}
	}
	if err := s.SendDTMF(context.Background(), "12*#wW"); err != nil {
		t.Errorf("valid digits rejected: %v", err)
	}
}
