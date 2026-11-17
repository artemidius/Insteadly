package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/artemidius/insteadly/internal/core/domain"
)

var (
	// errMissingEnded is a provider-contract failure: the event channel closed without a
	// terminal EventEnded. The runner never treats that as a successful call.
	errMissingEnded = errors.New("runner: call event channel closed without a terminal event")
	// errProviderContract marks an impossible/skipped telephony event sequence.
	errProviderContract = errors.New("runner: provider contract violation")
	// errReadinessTimeout means the optional media-stream readiness hook never fired.
	errReadinessTimeout = errors.New("runner: media stream did not become ready")
	// errProviderFailure means the provider reported an error for this attempt.
	errProviderFailure = errors.New("runner: provider reported a call error")
)

// eventRelay is the single post-answer consumer of the telephony event channel. It never
// drives order transitions; it forwards the terminal event to the agent, records the
// terminal reason/provider error, and — per the port contract — keeps draining until the
// adapter closes the channel. shutdown() is the bounded escape hatch for a broken adapter
// that never closes the channel, so no relay goroutine can outlive Run/Close.
type eventRelay struct {
	log *slog.Logger

	mu            sync.Mutex
	terminalEvent domain.CallEvent
	haveTerm      bool
	providerErr   bool

	agentCh chan domain.CallEvent
	done    chan struct{}

	stopCtx  context.Context
	stop     context.CancelFunc
	stopOnce sync.Once
}

func newEventRelay(log *slog.Logger) *eventRelay {
	stopCtx, stop := context.WithCancel(context.Background())
	return &eventRelay{
		log:     log,
		agentCh: make(chan domain.CallEvent, 1),
		done:    make(chan struct{}),
		stopCtx: stopCtx,
		stop:    stop,
	}
}

// agentChannel delivers the terminal event (if any) and is closed when the relay stops.
func (r *eventRelay) agentChannel() <-chan domain.CallEvent { return r.agentCh }

// run drains events until the channel closes or shutdown is requested. EventEnded is
// recorded and forwarded, but draining continues until channel closure as the port
// requires; shutdown() bounds a provider that never closes the channel.
func (r *eventRelay) run(events <-chan domain.CallEvent) {
	defer close(r.done)
	defer close(r.agentCh)
	for {
		select {
		case <-r.stopCtx.Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			r.handle(ev)
		}
	}
}

func (r *eventRelay) handle(ev domain.CallEvent) {
	switch ev.Kind {
	case domain.EventEnded:
		r.mu.Lock()
		r.terminalEvent = ev
		r.haveTerm = true
		r.mu.Unlock()
		select {
		case r.agentCh <- ev:
		default:
		}
	case domain.EventError:
		r.mu.Lock()
		r.providerErr = true
		r.mu.Unlock()
	case domain.EventDTMF:
		r.log.Debug("runner: dtmf ignored", "call_id", ev.CallID, "dtmf_len", len(ev.DTMF))
	case domain.EventDialing, domain.EventRinging, domain.EventAnswered:
		// Late progress after the agent started; the order already reflects it.
	default:
		r.log.Warn("runner: ignoring unexpected call event", "kind", ev.Kind)
	}
}

// wait blocks until the relay stops consuming or the bounded wait context expires.
func (r *eventRelay) wait(ctx context.Context) {
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

// shutdown cancels the drain and waits for the relay goroutine to exit. It is idempotent
// and safe on every Run/Close exit path.
func (r *eventRelay) shutdown() {
	r.stopOnce.Do(r.stop)
	<-r.done
}

func (r *eventRelay) terminal() (domain.CallEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminalEvent, r.haveTerm
}

func (r *eventRelay) providerError() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerErr
}
