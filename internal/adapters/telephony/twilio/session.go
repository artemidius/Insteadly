package twilio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
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
	done   chan struct{}

	// Outbound playback ordering. enqMu serializes SendAudio/MarkPlayback/ClearAudio so
	// a clear either drops an earlier audio item or is enqueued before later audio. A
	// single writer goroutine owns media/mark/clear wire writes, so their order is the
	// queue order. playEpoch invalidates audio accepted before the last clear.
	enqMu     sync.Mutex
	outQ      chan outboundItem
	playEpoch atomic.Uint64

	epochMu sync.Mutex
	epochCh chan struct{}

	streamReady     chan struct{}
	streamReadyOnce sync.Once

	finishOnce sync.Once
	endReason  domain.EndReason
	reaper     *time.Timer
	enc        frameEncoder
}

// outboundItem is one ordered outbound playback action: media, mark, or clear.
type outboundItem struct {
	kind  outboundKind
	data  []byte
	name  string
	epoch uint64
	done  chan error // buffered one; completed by the writer
}

type outboundKind int

const (
	itemMedia outboundKind = iota
	itemMark
	itemClear
)

// outQLimit bounds queued outbound playback actions. It matches the M0 queue depth of
// about two seconds of 20 ms frames.
const outQLimit = 100

func newOutboundItem(kind outboundKind) outboundItem {
	return outboundItem{kind: kind, done: make(chan error, 1)}
}

func (s *session) completeItem(item outboundItem, err error) {
	select {
	case item.done <- err:
	default:
	}
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
		outQ:          make(chan outboundItem, outQLimit),
		done:          make(chan struct{}),
		epochCh:       make(chan struct{}),
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
		case item := <-s.outQ:
			s.processOutbound(item)
		case <-s.done:
			// Drop whatever is still queued; the call can no longer play it.
			for {
				select {
				case item := <-s.outQ:
					s.completeItem(item, ErrSessionEnded)
				default:
					return
				}
			}
		}
	}
}

// processOutbound performs one ordered playback action and reports its result to the
// enqueuer. Media is dropped when a clear has already advanced the playback epoch.
func (s *session) processOutbound(item outboundItem) {
	var err error
	switch item.kind {
	case itemMedia:
		err = s.play(item.data, item.epoch)
	case itemMark:
		err = s.playMark(item.name)
	case itemClear:
		// The writer is the encoder's single owner: drop residual bytes from the cleared
		// generation before any post-clear media is encoded.
		s.enc = frameEncoder{}
		err = s.playClear(item.epoch)
	}
	if err != nil && !errors.Is(err, ErrSessionEnded) {
		s.log.Warn("twilio: outbound playback action failed", "kind", item.kind, "error", err)
	}
	s.completeItem(item, err)
}

// play sends every encoded frame for one accepted chunk unless the playback epoch has
// moved on. Only the writeLoop calls play, so the encoder has a single owner.
func (s *session) play(pcm []byte, epoch uint64) error {
	if epoch != s.playEpoch.Load() {
		return nil
	}
	for _, frame := range s.enc.encode(pcm) {
		for {
			sc, changed := s.currentStream()
			if sc == nil {
				// Wait for a reconnect, call end, or a clear that invalidates this audio.
				select {
				case <-s.done:
					return ErrSessionEnded
				case <-changed:
				case <-s.epochNotification():
				}
				if epoch != s.playEpoch.Load() {
					return nil
				}
				continue
			}
			if epoch != s.playEpoch.Load() {
				return nil
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

// epochNotification returns the channel closed when the playback epoch advances.
func (s *session) epochNotification() <-chan struct{} {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()
	return s.epochCh
}

// bumpEpoch invalidates all audio accepted before the next clear and wakes a writer that
// is waiting for a media stream.
func (s *session) bumpEpoch() uint64 {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()
	epoch := s.playEpoch.Add(1)
	close(s.epochCh)
	s.epochCh = make(chan struct{})
	return epoch
}

// playMark sends one mark after all previously queued media. With no attached stream the
// audio was never provider-played, so the mark cannot be acknowledged.
func (s *session) playMark(name string) error {
	sc := s.currentStreamOnly()
	if sc == nil {
		if s.isEnded() {
			return ErrSessionEnded
		}
		return ErrSessionNotReady
	}
	return sc.writeMark(name)
}

// playClear sends the wire clear. With no attached stream the local queue/encoder purge
// already satisfies the contract: an old disconnected Twilio buffer cannot outlive its
// old stream.
func (s *session) playClear(epoch uint64) error {
	sc := s.currentStreamOnly()
	if sc == nil {
		return nil
	}
	if err := sc.writeClear(); err != nil {
		if errors.Is(err, ErrSessionEnded) {
			return err
		}
		return fmt.Errorf("twilio: clear playback: %w", err)
	}
	return nil
}

// currentStreamOnly returns the attached stream without waiting for a reconnect.
func (s *session) currentStreamOnly() *streamConn {
	sc, _ := s.currentStream()
	return sc
}

// enqueue submits one outbound item under the ordering lock, honoring an already-ended
// call and the caller's deadline.
func (s *session) enqueue(ctx context.Context, item outboundItem) error {
	s.enqMu.Lock()
	defer s.enqMu.Unlock()
	select {
	case <-s.done:
		return ErrSessionEnded
	default:
	}
	select {
	case s.outQ <- item:
		return nil
	case <-s.done:
		return ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendAudio queues canonical PCM16 audio. It blocks while the queue is full (100 frames
// = 2 s) and fails once the call has ended.
func (s *session) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	if len(chunk.Data) == 0 {
		return nil
	}
	item := newOutboundItem(itemMedia)
	item.data = append([]byte(nil), chunk.Data...)
	item.epoch = s.playEpoch.Load()
	return s.enqueue(ctx, item)
}

// ClearAudio increments the playback epoch, purges queued audio, resets residual encoder
// bytes, and sends the provider clear. Audio accepted afterwards is ordered after it.
func (s *session) ClearAudio(ctx context.Context) error {
	if s.isEnded() {
		return ErrSessionEnded
	}
	item := newOutboundItem(itemClear)
	s.enqMu.Lock()
	item.epoch = s.bumpEpoch()
	// Drop every queued action from the old generation; a mark from that generation is
	// no longer meaningful once the audio has been cleared.
	for {
		select {
		case dropped := <-s.outQ:
			s.completeItem(dropped, nil)
		default:
			goto purged
		}
	}
purged:
	select {
	case s.outQ <- item:
		s.enqMu.Unlock()
	case <-s.done:
		s.enqMu.Unlock()
		return ErrSessionEnded
	case <-ctx.Done():
		s.enqMu.Unlock()
		return ctx.Err()
	}
	// The bounded clear write is ordered behind in-flight media by the writer.
	select {
	case err := <-item.done:
		return err
	case <-s.done:
		return ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MarkPlayback queues an opaque marker after all previously queued audio. It does not
// emit a local event: only a provider mark is playback evidence.
func (s *session) MarkPlayback(ctx context.Context, playbackID string) error {
	if playbackID == "" || len(playbackID) > 64 {
		return fmt.Errorf("twilio: invalid playback mark id")
	}
	if s.isEnded() {
		return ErrSessionEnded
	}
	item := newOutboundItem(itemMark)
	item.name = playbackID
	item.epoch = s.playEpoch.Load()
	if err := s.enqueue(ctx, item); err != nil {
		return err
	}
	select {
	case err := <-item.done:
		return err
	case <-s.done:
		return ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleMark normalizes one inbound provider mark.
func (s *session) handleMark(name string) {
	if name == "" || len(name) > 64 {
		s.log.Debug("twilio: ignoring malformed playback mark")
		return
	}
	s.postEvent(domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: name})
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
