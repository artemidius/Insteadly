package runner

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
)

// relayGoroutineCount counts live post-answer relay goroutines directly from stack
// traces, so the test proves they exit without depending on other test goroutines.
func relayGoroutineCount() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*eventRelay).run")
}

func TestRunSilentHangupDoesNotLeakRelay(t *testing.T) {
	order := testOrder()
	store := newFakeStore(order)
	session := newFakeSession()
	session.autoEndOnHangup = false // broken adapter: neither EventEnded nor channel close
	preload(session, progress(true)...)
	ag := newFakeAgent(domain.Outcome{}, agent.ErrCallEnded)
	r := testRunner(t, store, &fakeTelephony{session: session}, ag)

	start := time.Now()
	got, err := r.Run(context.Background(), order.ID)
	elapsed := time.Since(start)
	if !errors.Is(err, errMissingEnded) {
		t.Fatalf("err = %v, want errMissingEnded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("silent hangup took %v; the drain timeout did not bound cleanup", elapsed)
	}
	deadline := time.Now().Add(time.Second)
	for relayGoroutineCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := relayGoroutineCount(); n != 0 {
		t.Fatalf("%d post-answer relay goroutine(s) leaked after Run returned", n)
	}
	if got.Outcome == nil || got.Outcome.Kind != domain.OutcomeFailed || got.Outcome.FailReason != "error" {
		t.Fatalf("final = %+v", got)
	}
	if session.hangups() != 1 {
		t.Errorf("hangups = %d, want 1", session.hangups())
	}
}
