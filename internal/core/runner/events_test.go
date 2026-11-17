package runner

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
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
