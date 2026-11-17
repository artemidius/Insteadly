package agent

import (
	"sync"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// controlRouter is the agent's single consumer of the runner's bounded post-answer
// control relay. Playback marks go to the one segment awaiting acknowledgement; inbound
// DTMF becomes bounded evidence only and is never echoed back to the far end.
type controlRouter struct {
	control <-chan domain.CallEvent
	marks   chan string
	errs    chan error
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once

	mu   sync.Mutex
	dtmf []string
}

func newControlRouter(control <-chan domain.CallEvent) *controlRouter {
	r := &controlRouter{
		control: control,
		marks:   make(chan string, markBufferSize),
		errs:    make(chan error, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	return r
}

func (r *controlRouter) start() { go r.run() }

func (r *controlRouter) run() {
	defer close(r.done)
	defer close(r.marks)
	for {
		select {
		case <-r.stop:
			return
		case ev, open := <-r.control:
			if !open {
				return
			}
			switch ev.Kind {
			case domain.EventPlaybackMark:
				select {
				case r.marks <- ev.PlaybackID:
				default:
					// Losing playback evidence silently is forbidden.
					select {
					case r.errs <- errControlOverflow:
					default:
					}
					return
				}
			case domain.EventDTMF:
				r.pushDTMF(ev.DTMF)
			default:
				// The runner owns terminal/progress events; nothing else belongs here.
			}
		}
	}
}

func (r *controlRouter) pushDTMF(digit string) {
	if digit == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dtmf = append(r.dtmf, digit)
	if len(r.dtmf) > dtmfRingSize {
		r.dtmf = r.dtmf[len(r.dtmf)-dtmfRingSize:]
	}
}

func (r *controlRouter) marksChannel() <-chan string  { return r.marks }
func (r *controlRouter) errChannel() <-chan error     { return r.errs }
func (r *controlRouter) doneChannel() <-chan struct{} { return r.done }

// dtmfSnapshot returns bounded inbound DTMF evidence. It is never sent back automatically.
func (r *controlRouter) dtmfSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dtmf...)
}

// stop terminates the router without closing the producer-owned control channel.
func (r *controlRouter) stopRouter() {
	r.once.Do(func() { close(r.stop) })
	<-r.done
}
