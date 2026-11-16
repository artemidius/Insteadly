// Package stub provides deterministic fake providers. They exist for two reasons:
// make the C0 binary fully wired without any external service, and serve as permanent
// test doubles for the core in later chunks.
//
// Fake providers for development and tests; not for production.
package stub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Telephony is a fake telephony provider: every Dial succeeds and walks the fixed
// event sequence dialing -> ringing -> answered -> (hangup) ended.
type Telephony struct {
	log *slog.Logger
}

var _ ports.Telephony = (*Telephony)(nil)

// NewTelephony returns a fake telephony provider. A nil logger discards output.
func NewTelephony(log *slog.Logger) *Telephony {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Telephony{log: log}
}

// Dial validates that the order has a clinic phone number, then returns a live fake
// session. The fake never fails to reach the far end: success is the point.
func (t *Telephony) Dial(ctx context.Context, order domain.Order) (ports.CallSession, error) {
	if order.Clinic.Phone == "" {
		return nil, fmt.Errorf("stub telephony: order %s: clinic phone is required", order.ID)
	}
	s := &callSession{
		id:      domain.NewCallID(),
		log:     t.log,
		events:  make(chan domain.CallEvent, 16),
		inbound: make(chan domain.AudioChunk, 16),
		done:    make(chan struct{}),
	}
	go s.run(ctx)
	return s, nil
}

type callSession struct {
	id      domain.CallID
	log     *slog.Logger
	events  chan domain.CallEvent
	inbound chan domain.AudioChunk
	done    chan struct{}

	hangupOnce sync.Once
}

var _ ports.CallSession = (*callSession)(nil)

func (s *callSession) ID() domain.CallID               { return s.id }
func (s *callSession) Events() <-chan domain.CallEvent { return s.events }
func (s *callSession) Inbound() <-chan domain.AudioChunk {
	return s.inbound
}

// run owns both output channels: it is the only goroutine that closes them, which keeps
// sends and closes free of races with Hangup.
func (s *callSession) run(ctx context.Context) {
	defer close(s.events)
	defer close(s.inbound)

	for _, kind := range []domain.CallEventKind{domain.EventDialing, domain.EventRinging, domain.EventAnswered} {
		s.events <- domain.CallEvent{CallID: s.id, Kind: kind, At: time.Now()}
	}
	// One frame of digital silence proves the canonical-audio plumbing works.
	s.inbound <- domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}

	reason := domain.EndAborted
	select {
	case <-s.done:
		reason = domain.EndCompleted
	case <-ctx.Done():
	}
	s.events <- domain.CallEvent{CallID: s.id, Kind: domain.EventEnded, EndReason: reason, At: time.Now()}
}

// SendAudio discards playback audio (the fake has no far end).
func (s *callSession) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	return nil // the fake has no far end; playback is always accepted
}

// SendDTMF logs the digits at debug level and does nothing else.
func (s *callSession) SendDTMF(ctx context.Context, digits string) error {
	s.log.Debug("stub telephony: dtmf ignored", "call_id", s.id, "digits", digits)
	return nil
}

// Hangup ends the call. Idempotent and safe from multiple goroutines.
func (s *callSession) Hangup(ctx context.Context) error {
	s.hangupOnce.Do(func() { close(s.done) })
	return nil
}
