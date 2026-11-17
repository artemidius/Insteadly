package twilio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Adapter-local failures the core may see. They are deliberately not ports sentinels:
// they describe this transport, not a domain condition.
var (
	ErrSessionEnded    = errors.New("twilio: call session ended")
	ErrSessionNotReady = errors.New("twilio: call session not ready")
	ErrDTMFUnsupported = errors.New("twilio: outbound dtmf is not supported")
)

const (
	// sessionMaxDuration is a leak safety net, not a product policy. The scheduler (C5)
	// will own per-call time limits.
	sessionMaxDuration = 30 * time.Minute
	// eventDeliveryTimeout bounds a terminal-event send to a stalled consumer.
	eventDeliveryTimeout = 5 * time.Second
	// endedSessionRetention keeps a finished session addressable for a short while so
	// duplicate/late Twilio callbacks and an already-fetched voice URL still get a clean
	// response instead of a 404 that Twilio treats as an error.
	endedSessionRetention = 5 * time.Minute
)

var (
	dtmfPattern  = regexp.MustCompile(`^[0-9A-D*#wW]{1,32}$`)
	dtmfDigit    = regexp.MustCompile(`^[0-9A-D*#]$`)
	e164Pattern  = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	statusEvents = map[string]domain.EndReason{
		"completed": domain.EndCompleted,
		"busy":      domain.EndBusy,
		"no-answer": domain.EndNoAnswer,
		"canceled":  domain.EndAborted,
		"failed":    domain.EndError,
	}
)

// session implements ports.CallSession. One session can own several WebSocket streams
// over its life (reconnect, DTMF redirect), but exactly one call.
type session struct {
	adapter *Adapter
	id      domain.CallID
	log     *slog.Logger

	mu            sync.Mutex
	sid           string
	stream        *streamConn
	streamChanged chan struct{}

	// progMu serializes progress events so dialing -> ringing -> answered stays ordered
	// even when status callbacks arrive concurrently and out of order.
	progMu   sync.Mutex
	dialing  bool
	ringing  bool
	answered bool

	inMu           sync.Mutex
	inbound        chan domain.AudioChunk
	inClosed       bool
	inboundDropped int

	events chan domain.CallEvent
	evq    chan domain.CallEvent
	outQ   chan domain.AudioChunk
	done   chan struct{}

	streamReady     chan struct{}
	streamReadyOnce sync.Once

	finishOnce sync.Once
	endReason  domain.EndReason
	reaper     *time.Timer
	enc        frameEncoder
}

var _ ports.CallSession = (*session)(nil)

func newSession(a *Adapter, id domain.CallID) *session {
	s := &session{
		adapter:       a,
		id:            id,
		log:           a.log.With("call_id", string(id)),
		streamChanged: make(chan struct{}),
		events:        make(chan domain.CallEvent, 64),
		evq:           make(chan domain.CallEvent, 64),
		inbound:       make(chan domain.AudioChunk, 100),
		outQ:          make(chan domain.AudioChunk, 100),
		done:          make(chan struct{}),
		streamReady:   make(chan struct{}),
	}
	s.reaper = time.AfterFunc(sessionMaxDuration, func() {
		s.log.Warn("twilio: call exceeded maximum duration")
		s.finish(domain.EndError)
		if sid := s.providerSID(); sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.endCall(ctx, sid); err != nil {
				s.log.Warn("twilio: reaper hangup failed", "error", err)
			}
		}
	})
	go s.dispatch()
	go s.writeLoop()
	return s
}

func (s *session) ID() domain.CallID               { return s.id }
func (s *session) Events() <-chan domain.CallEvent { return s.events }

func (s *session) Inbound() <-chan domain.AudioChunk { return s.inbound }

// StreamReady is an adapter-specific diagnostic (not part of ports.CallSession): it
// closes when the first Media Stream attaches. The live test uses it to tell "Twilio
// stripped <Stream> on a trial account" apart from "audio is silent".
func (s *session) StreamReady() <-chan struct{} { return s.streamReady }

func (s *session) setProviderSID(sid string) {
	s.mu.Lock()
	s.sid = sid
	s.mu.Unlock()
}

func (s *session) providerSID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

func (s *session) isEnded() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) isAnswered() bool {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	return s.answered
}

// progress emits one normalized progress event at most once, filling gaps caused by
// out-of-order callbacks (Twilio does not guarantee callback ordering).
func (s *session) progress(kind domain.CallEventKind) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	if s.isEnded() {
		return
	}
	switch kind {
	case domain.EventDialing:
		if s.dialing || s.ringing || s.answered {
			return
		}
		s.dialing = true
	case domain.EventRinging:
		if s.ringing || s.answered {
			return
		}
		if !s.dialing {
			s.dialing = true
			s.postEvent(domain.CallEvent{Kind: domain.EventDialing})
		}
		s.ringing = true
	case domain.EventAnswered:
		if s.answered {
			return
		}
		if !s.dialing {
			s.dialing = true
			s.postEvent(domain.CallEvent{Kind: domain.EventDialing})
		}
		if !s.ringing {
			s.ringing = true
			s.postEvent(domain.CallEvent{Kind: domain.EventRinging})
		}
		s.answered = true
	default:
		return
	}
	s.postEvent(domain.CallEvent{Kind: kind})
}

func (s *session) handleStatus(status string) {
	switch status {
	case "queued", "initiated":
		s.progress(domain.EventDialing)
	case "ringing":
		s.progress(domain.EventRinging)
	case "in-progress":
		s.progress(domain.EventAnswered)
	case "completed", "busy", "no-answer", "canceled", "failed":
		s.finish(statusEvents[status])
	default:
		s.log.Debug("twilio: unknown call status", "status", status)
	}
}

func (s *session) handleDTMF(digit string) {
	if !dtmfDigit.MatchString(digit) {
		s.log.Debug("twilio: ignoring malformed dtmf", "digit", digit)
		return
	}
	s.postEvent(domain.CallEvent{Kind: domain.EventDTMF, DTMF: digit})
}

func (s *session) postEvent(ev domain.CallEvent) {
	ev.CallID = s.id
	ev.At = time.Now()
	select {
	case s.evq <- ev:
	case <-s.done:
	}
}

func (s *session) dispatch() {
	defer func() {
		s.closeInbound()
		close(s.events)
	}()
	for {
		select {
		case ev := <-s.evq:
			s.sendEvent(ev)
		case <-s.done:
			for {
				select {
				case ev := <-s.evq:
					s.sendEvent(ev)
				default:
					s.sendEvent(domain.CallEvent{
						CallID:    s.id,
						Kind:      domain.EventEnded,
						EndReason: s.endReason,
						At:        time.Now(),
					})
					return
				}
			}
		}
	}
}

func (s *session) sendEvent(ev domain.CallEvent) {
	select {
	case s.events <- ev:
	case <-time.After(eventDeliveryTimeout):
		s.log.Warn("twilio: event consumer stalled", "kind", ev.Kind)
	}
}

// finish ends the session exactly once and reports whether this call performed the
// transition (false means it had already ended naturally).
func (s *session) finish(reason domain.EndReason) bool {
	ran := false
	s.finishOnce.Do(func() {
		ran = true
		if reason == domain.EndCompleted {
			s.progress(domain.EventAnswered)
		}
		s.endReason = reason
		if s.reaper != nil {
			s.reaper.Stop()
		}
		s.detachAndCloseStream()
		close(s.done)
		time.AfterFunc(endedSessionRetention, func() { s.adapter.remove(s.id) })
	})
	return ran
}

func (s *session) detachAndCloseStream() {
	s.mu.Lock()
	sc := s.stream
	s.stream = nil
	s.mu.Unlock()
	if sc != nil {
		sc.close()
	}
}

func (s *session) attachStream(sc *streamConn) {
	if s.isEnded() {
		sc.close()
		return
	}
	s.mu.Lock()
	old := s.stream
	s.stream = sc
	close(s.streamChanged)
	s.streamChanged = make(chan struct{})
	s.mu.Unlock()
	s.streamReadyOnce.Do(func() { close(s.streamReady) })
	if old != nil && old != sc {
		old.close()
	}
	if s.isEnded() {
		s.detachStream(sc)
		sc.close()
		return
	}
	s.progress(domain.EventAnswered)
}

func (s *session) detachStream(sc *streamConn) {
	s.mu.Lock()
	if s.stream == sc {
		s.stream = nil
	}
	s.mu.Unlock()
}

func (s *session) currentStream() (*streamConn, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream, s.streamChanged
}

func (s *session) waitStream() *streamConn {
	for {
		sc, changed := s.currentStream()
		if sc != nil {
			return sc
		}
		select {
		case <-s.done:
			return nil
		case <-changed:
		}
	}
}

func (s *session) pushInbound(chunk domain.AudioChunk) {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	if s.inClosed {
		return
	}
	select {
	case s.inbound <- chunk:
	default:
		// Drop the oldest canonical frame rather than blocking the WebSocket reader.
		select {
		case <-s.inbound:
			s.inboundDropped++
		default:
		}
		s.inbound <- chunk
	}
}

func (s *session) closeInbound() {
	s.inMu.Lock()
	dropped := s.inboundDropped
	if !s.inClosed {
		s.inClosed = true
		close(s.inbound)
	}
	s.inMu.Unlock()
	if dropped > 0 {
		s.log.Warn("twilio: dropped inbound audio frames", "frames", dropped)
	}
}

func (s *session) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		case chunk := <-s.outQ:
			if err := s.play(chunk.Data); err != nil {
				if errors.Is(err, ErrSessionEnded) {
					return
				}
				s.log.Warn("twilio: playback failed", "error", err)
			}
		}
	}
}

func (s *session) play(pcm []byte) error {
	for _, frame := range s.enc.encode(pcm) {
		for {
			sc := s.waitStream()
			if sc == nil {
				return ErrSessionEnded
			}
			if err := sc.writeMedia(frame); err == nil {
				break
			} else {
				s.log.Debug("twilio: stream write failed", "error", err)
			}
			s.detachStream(sc)
			sc.close()
		}
	}
	return nil
}

// SendAudio queues canonical PCM16 audio. It blocks while the queue is full (100 frames
// = 2 s) and fails once the call has ended.
func (s *session) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	if len(chunk.Data) == 0 {
		return nil
	}
	if s.isEnded() {
		return ErrSessionEnded
	}
	select {
	case <-s.done:
		return ErrSessionEnded
	case s.outQ <- chunk:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendDTMF sends digits through an answered call. Twilio's Media Streams socket has no
// outbound DTMF message, so the call is redirected to TwiML that plays the tones and
// reconnects the bidirectional stream.
func (s *session) SendDTMF(ctx context.Context, digits string) error {
	if !dtmfPattern.MatchString(digits) {
		return fmt.Errorf("twilio: invalid dtmf sequence")
	}
	if s.isEnded() {
		return ErrSessionEnded
	}
	if !s.isAnswered() {
		return ErrSessionNotReady
	}
	sid := s.providerSID()
	if sid == "" {
		return ErrSessionNotReady
	}
	twiml, err := s.adapter.dtmfTwiML(s.id, digits)
	if err != nil {
		return fmt.Errorf("twilio: build dtmf twiml: %w", err)
	}
	if err := s.adapter.updateCall(ctx, sid, string(twiml)); err != nil {
		return err
	}
	return nil
}

// Hangup ends the call locally first, then asks Twilio to hang up. It is idempotent and
// safe after a natural end.
func (s *session) Hangup(ctx context.Context) error {
	if !s.finish(domain.EndAborted) {
		// The call already ended (natural terminal callback); don't poke Twilio again.
		return nil
	}
	sid := s.providerSID()
	if sid == "" {
		return nil
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.adapter.endCall(hctx, sid); err != nil {
		s.log.Warn("twilio: hangup request failed", "error", err)
	}
	return nil
}
