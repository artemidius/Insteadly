package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
)

func relayTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEventRelayShutdownStopsBlockedDrain(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent) // stays open forever
	go relay.run(events)

	done := make(chan struct{})
	go func() {
		relay.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay goroutine did not stop after shutdown")
	}
	if _, have := relay.terminal(); have {
		t.Error("blocked relay recorded a terminal event")
	}
}

func TestEventRelayDrainsUntilChannelClose(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent, 2)
	events <- domain.CallEvent{Kind: domain.EventEnded, EndReason: domain.EndBusy}
	events <- domain.CallEvent{Kind: domain.EventError}
	go relay.run(events)

	select {
	case ev := <-relay.agentChannel():
		if ev.Kind != domain.EventEnded || ev.EndReason != domain.EndBusy {
			t.Fatalf("forwarded event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event was not forwarded")
	}

	// EventEnded is final but does not close the port's channel; the relay must keep
	// draining until the adapter closes it.
	select {
	case <-relay.done:
		t.Fatal("relay exited on EventEnded before the event channel closed")
	case <-time.After(20 * time.Millisecond):
	}

	close(events)
	select {
	case <-relay.done:
	case <-time.After(time.Second):
		t.Fatal("relay did not exit after the event channel closed")
	}
	if ev, have := relay.terminal(); !have || ev.EndReason != domain.EndBusy {
		t.Errorf("terminal = %+v, have=%v", ev, have)
	}
	if !relay.providerError() {
		t.Error("post-terminal EventError was not drained/recorded")
	}
}

func TestEventRelayCloseWithoutTerminal(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent)
	close(events)
	go relay.run(events)
	select {
	case <-relay.done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop on channel closure")
	}
	if _, have := relay.terminal(); have {
		t.Error("closed channel without EventEnded recorded a terminal event")
	}
}

func TestEventRelayForwardsDTMFAndMarksInOrder(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent, 4)
	events <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"}
	events <- domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: "m1"}
	events <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "2"}
	go relay.run(events)

	want := []struct {
		kind domain.CallEventKind
		id   string
	}{{domain.EventDTMF, "1"}, {domain.EventPlaybackMark, "m1"}, {domain.EventDTMF, "2"}}
	for i, w := range want {
		select {
		case ev := <-relay.controlChannel():
			if ev.Kind != w.kind {
				t.Fatalf("event %d kind = %q, want %q", i, ev.Kind, w.kind)
			}
			id := ev.DTMF
			if ev.Kind == domain.EventPlaybackMark {
				id = ev.PlaybackID
			}
			if id != w.id {
				t.Fatalf("event %d id = %q, want %q", i, id, w.id)
			}
		case <-time.After(time.Second):
			t.Fatal("control event not forwarded")
		}
	}
	close(events)
	select {
	case <-relay.done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
}

func TestEventRelayControlOverflowClosesAndErrors(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent, controlBuffer+1)
	for i := 0; i < controlBuffer+1; i++ {
		events <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"}
	}
	close(events)
	go relay.run(events)
	select {
	case <-relay.done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not drain")
	}
	if !errors.Is(relay.controlError(), errControlOverflow) {
		t.Fatalf("control overflow not recorded: %v", relay.controlError())
	}
	// The bounded control channel was closed rather than silently dropping a digit.
	drained := 0
	for range relay.controlChannel() {
		drained++
	}
	if drained != controlBuffer {
		t.Errorf("forwarded before overflow = %d, want %d", drained, controlBuffer)
	}
}

func TestEventRelayTerminalIndependentOfFullControlQueue(t *testing.T) {
	relay := newEventRelay(relayTestLogger())
	events := make(chan domain.CallEvent, controlBuffer+1)
	for i := 0; i < controlBuffer; i++ {
		events <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"}
	}
	events <- domain.CallEvent{Kind: domain.EventEnded, EndReason: domain.EndBusy}
	go relay.run(events)

	select {
	case ev := <-relay.agentChannel():
		if ev.Kind != domain.EventEnded || ev.EndReason != domain.EndBusy {
			t.Fatalf("terminal = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event was blocked behind the control queue")
	}
	close(events)
	<-relay.done
}

func TestPreAnswerPlaybackMarkIsAProviderContractError(t *testing.T) {
	store := newFakeStore(testOrder())
	store.orders = map[domain.OrderID]domain.Order{}
	order := testOrder()
	store = newFakeStore(order)
	telephony := &fakeTelephony{session: newFakeSession()}
	r := testRunner(t, store, telephony, newFakeAgent(domain.Outcome{}, nil))
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	telephony.session.send(domain.CallEvent{Kind: domain.EventDialing})
	telephony.session.send(domain.CallEvent{Kind: domain.EventRinging})
	telephony.session.send(domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: "early"})
	select {
	case err := <-done:
		if !errors.Is(err, errProviderContract) {
			t.Fatalf("err = %v, want errProviderContract", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not reject a pre-answer playback mark")
	}
}

func TestRunnerUsesAgentIVRDeadEndOutcome(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	telephony := &fakeTelephony{session: newFakeSession()}
	agentOutcome := domain.Outcome{Kind: domain.OutcomeFailed, FailReason: "ivr_dead_end", Detail: "fixed"}
	ag := newFakeAgent(agentOutcome, nil)
	r := testRunner(t, store, telephony, ag)
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	telephony.session.send(domain.CallEvent{Kind: domain.EventDialing})
	telephony.session.send(domain.CallEvent{Kind: domain.EventRinging})
	telephony.session.send(domain.CallEvent{Kind: domain.EventAnswered})
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	final, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Outcome == nil || final.Outcome.FailReason != "ivr_dead_end" {
		t.Fatalf("outcome = %+v", final.Outcome)
	}
	if store.finishCount != 1 {
		t.Errorf("finish count = %d, want 1", store.finishCount)
	}
	if telephony.session.hangups() != 1 {
		t.Errorf("hangup count = %d, want 1", telephony.session.hangups())
	}
}

func TestRunnerControlOverflowNeverPersistsAValidOutcome(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	ag := newFakeAgent(bookedOutcome(), nil)
	ag.session = session
	ag.onRun = func(_ context.Context, s *fakeSession) {
		for i := 0; i < controlBuffer+5; i++ {
			s.send(domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"})
		}
	}
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	session.send(domain.CallEvent{Kind: domain.EventDialing})
	session.send(domain.CallEvent{Kind: domain.EventRinging})
	session.send(domain.CallEvent{Kind: domain.EventAnswered})
	select {
	case err := <-done:
		if !errors.Is(err, errControlOverflow) {
			t.Fatalf("err = %v, want errControlOverflow", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not return")
	}
	final, err := store.Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Outcome == nil || final.Outcome.Kind != domain.OutcomeFailed || final.Outcome.FailReason != "error" {
		t.Fatalf("a valid outcome was persisted after a control drop: %+v", final.Outcome)
	}
	if store.finishCount != 1 {
		t.Errorf("finish count = %d, want 1", store.finishCount)
	}
	if session.hangups() != 1 {
		t.Errorf("hangup count = %d, want 1", session.hangups())
	}
}

// recordingPublisher captures events for assertions.
type recordingPublisher struct {
	mu   sync.Mutex
	got  []events.OrderEvent
	note chan struct{}
}

func newRecordingPublisher() *recordingPublisher {
	return &recordingPublisher{note: make(chan struct{}, 1)}
}

func (p *recordingPublisher) Publish(_ context.Context, ev events.OrderEvent) error {
	p.mu.Lock()
	p.got = append(p.got, ev)
	p.mu.Unlock()
	select {
	case p.note <- struct{}{}:
	default:
	}
	return nil
}

func (p *recordingPublisher) events() []events.OrderEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.OrderEvent(nil), p.got...)
}

func (p *recordingPublisher) waitForStatus(t *testing.T, status domain.OrderStatus) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		for _, ev := range p.events() {
			if ev.Status == status {
				return
			}
		}
		select {
		case <-p.note:
		case <-deadline:
			t.Fatalf("event %q was not published", status)
		}
	}
}

func testRunnerWithEvents(t *testing.T, store *fakeStore, telephony *fakeTelephony, ag agent.Engine, pub events.Publisher) *Runner {
	t.Helper()
	r, err := New(Config{
		Telephony:      telephony,
		Orders:         store,
		Agent:          ag,
		Events:         pub,
		CleanupTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestRunnerPublishesStatusEvents(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	pub := newRecordingPublisher()
	r := testRunnerWithEvents(t, store, &fakeTelephony{session: session}, newFakeAgent(bookedOutcome(), nil), pub)

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	session.send(domain.CallEvent{Kind: domain.EventDialing})
	session.send(domain.CallEvent{Kind: domain.EventRinging})
	session.send(domain.CallEvent{Kind: domain.EventAnswered})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not return")
	}

	got := pub.events()
	want := []domain.OrderStatus{domain.OrderDialing, domain.OrderRinging, domain.OrderInCall, domain.OrderFinished}
	if len(got) != len(want) {
		t.Fatalf("events = %d (%v), want %d", len(got), statusesOf(got), len(want))
	}
	for i, status := range want {
		if got[i].Status != status {
			t.Errorf("event[%d] status = %q, want %q", i, got[i].Status, status)
		}
		if got[i].OrderID != order.ID {
			t.Errorf("event[%d] order = %s, want %s", i, got[i].OrderID, order.ID)
		}
		if got[i].Attempts != 1 {
			t.Errorf("event[%d] attempts = %d, want 1", i, got[i].Attempts)
		}
		if got[i].At.IsZero() {
			t.Errorf("event[%d] has no timestamp", i)
		}
	}
	if got[0].Outcome != nil {
		t.Errorf("dialing event carries an outcome: %+v", got[0].Outcome)
	}
	if got[3].Outcome == nil || got[3].Outcome.Kind != domain.OutcomeBooked {
		t.Errorf("finished event outcome = %+v, want booked", got[3].Outcome)
	}
}

func TestRunnerPublishesFailureEvent(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	pub := newRecordingPublisher()
	r := testRunnerWithEvents(t, store, &fakeTelephony{session: session}, newFakeAgent(domain.Outcome{}, nil), pub)

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	session.send(domain.CallEvent{Kind: domain.EventDialing})
	session.send(domain.CallEvent{Kind: domain.EventRinging})
	session.end(domain.EndBusy)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remote busy is an expected outcome, got err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not return")
	}

	got := pub.events()
	if len(got) != 3 {
		t.Fatalf("events = %v, want dialing, ringing, finished", statusesOf(got))
	}
	last := got[len(got)-1]
	if last.Status != domain.OrderFinished || last.Outcome == nil ||
		last.Outcome.Kind != domain.OutcomeFailed || last.Outcome.FailReason != "busy" {
		t.Fatalf("terminal event = %+v", last)
	}
}

func TestRunnerPublishesAbortedOnShutdown(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	pub := newRecordingPublisher()
	r := testRunnerWithEvents(t, store, &fakeTelephony{session: session}, newFakeAgent(domain.Outcome{}, nil), pub)

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), order.ID)
		done <- err
	}()
	pub.waitForStatus(t, domain.OrderDialing)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not return after Close")
	}
	got := pub.events()
	if len(got) < 2 {
		t.Fatalf("events = %v, want at least dialing+finished", statusesOf(got))
	}
	last := got[len(got)-1]
	if last.Status != domain.OrderFinished || last.Outcome == nil || last.Outcome.Kind != domain.OutcomeAborted {
		t.Fatalf("terminal event = %+v, want finished/aborted", last)
	}
}

func statusesOf(evs []events.OrderEvent) []domain.OrderStatus {
	out := make([]domain.OrderStatus, len(evs))
	for i, ev := range evs {
		out[i] = ev.Status
	}
	return out
}
