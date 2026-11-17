package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	maxSegmentRunes   = 240
	segmentSoftBreaks = ",;: "
)

// segmenter splits streamed assistant text into bounded, provider-speakable segments.
// It emits at sentence-ending punctuation/newlines and at 240 Unicode code points,
// preferring the last comma/semicolon/space. Only bounded segment text is retained.
type segmenter struct {
	buf []rune
}

// push appends one text delta and returns every complete segment it closes.
func (s *segmenter) push(delta string) []string {
	s.buf = append(s.buf, []rune(delta)...)
	var out []string
	for {
		seg, rest, ok := s.next()
		if !ok {
			return out
		}
		s.buf = rest
		if t := strings.TrimSpace(seg); t != "" {
			out = append(out, t)
		}
	}
}

// flush returns the final nonempty remainder.
func (s *segmenter) flush() string {
	t := strings.TrimSpace(string(s.buf))
	s.buf = s.buf[:0]
	return t
}

func (s *segmenter) next() (segment string, rest []rune, ok bool) {
	for i, r := range s.buf {
		if r == '.' || r == '?' || r == '!' || r == '\n' {
			return string(s.buf[:i+1]), append([]rune(nil), s.buf[i+1:]...), true
		}
	}
	if len(s.buf) <= maxSegmentRunes {
		return "", nil, false
	}
	// Prefer the last soft break within the bound, otherwise hard-split at the bound.
	cut := maxSegmentRunes
	for i := maxSegmentRunes - 1; i > maxSegmentRunes/2; i-- {
		if strings.ContainsRune(segmentSoftBreaks, s.buf[i]) {
			cut = i + 1
			break
		}
	}
	return string(s.buf[:cut]), append([]rune(nil), s.buf[cut:]...), true
}

// speechTracker lets the interruption path abort the TTS stream currently owned by a
// segment player. It never holds the stream across a blocking call.
type speechTracker struct {
	mu     sync.Mutex
	stream ports.SpeechStream
}

func (t *speechTracker) set(s ports.SpeechStream) {
	t.mu.Lock()
	t.stream = s
	t.mu.Unlock()
}

func (t *speechTracker) clear(s ports.SpeechStream) {
	t.mu.Lock()
	if t.stream == s {
		t.stream = nil
	}
	t.mu.Unlock()
}

// abort cancels the active TTS generation without flushing. A nil stream is a no-op.
func (t *speechTracker) abort(ctx context.Context) error {
	t.mu.Lock()
	s := t.stream
	t.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Abort(ctx)
}

// segmentPlayer plays known text segments one at a time: open TTS for exactly one
// segment, drain while Close flushes, mark playback after the last chunk, then require
// the normalized provider mark before treating the segment as confirmed assistant speech.
type segmentPlayer struct {
	agent   *Agent
	call    AudioSession
	pb      *playbackState
	control *controlRouter
	gen     uint64

	mu          sync.Mutex
	confirmed   []string
	spoke       bool
	audioFrames int
}

func newSegmentPlayer(a *Agent, call AudioSession, pb *playbackState, control *controlRouter) *segmentPlayer {
	return &segmentPlayer{agent: a, call: call, pb: pb, control: control, gen: pb.generation()}
}

// play speaks exactly one bounded segment and returns only after its playback mark was
// observed (or a provider/ack error occurred).
func (p *segmentPlayer) play(ctx context.Context, text, marker string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("agent: assistant segment is not valid UTF-8")
	}
	stream, err := p.agent.cfg.TTS.Speak(ctx, ports.SpeakOptions{})
	if err != nil {
		return fmt.Errorf("tts open: %w", err)
	}
	p.pb.tracker.set(stream)
	defer p.pb.tracker.clear(stream)

	feedErr := stream.FeedText(ctx, text)
	if feedErr != nil {
		// Close (not Abort) keeps already-accepted text's cleanup path identical to the
		// normal case; the agent reports the feed failure.
		_ = p.closeAndDrain(ctx, stream, nil)
		return fmt.Errorf("tts feed: %w", feedErr)
	}
	p.mu.Lock()
	p.spoke = true
	p.mu.Unlock()

	before := p.playbackFrames()
	drainDone := make(chan error, 1)
	go p.drain(ctx, stream.Audio(), drainDone)

	if err := p.closeAndDrain(ctx, stream, drainDone); err != nil {
		return err
	}
	// A barge/hold cancellation must not report unconfirmed text or ask for a mark.
	if err := ctx.Err(); err != nil {
		return err
	}
	// Playback evidence requires at least one actually forwarded nonempty canonical frame.
	// An empty provider synthesis must never unlock tool/outcome behavior or be recorded as
	// spoken assistant text.
	if p.playbackFrames() == before {
		return errNoPlaybackAudio
	}
	if err := p.call.MarkPlayback(ctx, marker); err != nil {
		if errors.Is(err, errTerminalDuringTurn) {
			return err
		}
		return fmt.Errorf("mark playback: %w", err)
	}
	if err := p.awaitMark(ctx, marker); err != nil {
		return err
	}
	p.mu.Lock()
	p.confirmed = append(p.confirmed, text)
	p.mu.Unlock()
	return nil
}

// closeAndDrain closes TTS (flushing normal text) while the drainer keeps consuming audio,
// then waits for the drainer. The cleanup context is independent of a canceled turn so a
// normal end-of-text flush is not skipped.
func (p *segmentPlayer) closeAndDrain(ctx context.Context, stream ports.SpeechStream, drainDone <-chan error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.agent.cfg.CleanupTimeout)
	defer cancel()
	closeErr := stream.Close(cleanupCtx)
	var drainErr error
	if drainDone != nil {
		select {
		case drainErr = <-drainDone:
		case <-cleanupCtx.Done():
			drainErr = cleanupCtx.Err()
		}
	}
	if err := errors.Join(closeErr, drainErr); err != nil {
		return fmt.Errorf("tts close: %w", err)
	}
	return nil
}

// drain forwards audio for this playback generation. Late/old-generation chunks are
// discarded rather than sent; it always keeps reading until Audio closes so TTS
// backpressure can never deadlock Close/Abort.
func (p *segmentPlayer) drain(ctx context.Context, audio <-chan domain.AudioChunk, done chan<- error) {
	var firstErr error
	for chunk := range audio {
		if firstErr != nil {
			continue
		}
		if ctx.Err() != nil || p.pb.generation() != p.gen {
			continue
		}
		if len(chunk.Data) == 0 {
			continue
		}
		if err := p.call.SendAudio(ctx, chunk); err != nil {
			if ctx.Err() != nil {
				firstErr = errTerminalDuringTurn
			} else {
				firstErr = fmt.Errorf("send audio: %w", err)
			}
			continue
		}
		p.notePlaybackFrame()
	}
	done <- firstErr
}

// awaitMark waits for the exact normalized mark for marker, ignoring late, duplicate, and
// out-of-order marks from older segments/generations.
func (p *segmentPlayer) awaitMark(ctx context.Context, marker string) error {
	timer := time.NewTimer(p.agent.cfg.PlaybackAckTimeout)
	defer timer.Stop()
	for {
		select {
		case id, ok := <-p.control.marksChannel():
			if !ok {
				return errControlClosed
			}
			if id == marker {
				return nil
			}
		case <-timer.C:
			return errPlaybackAckTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *segmentPlayer) confirmedText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.confirmed, " ")
}

func (p *segmentPlayer) confirmedSegments() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.confirmed...)
}

func (p *segmentPlayer) notePlaybackFrame() {
	p.mu.Lock()
	p.audioFrames++
	p.mu.Unlock()
}

func (p *segmentPlayer) playbackFrames() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.audioFrames
}

func (p *segmentPlayer) anySpoke() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spoke
}

func (s speechOutcome) confirmedText() string {
	return strings.Join(s.confirmed, " ")
}

// speechOutcome is one assistant turn's playback result. Confirmed contains only complete
// segments whose provider marks were observed before any clear.
type speechOutcome struct {
	spoke       bool
	confirmed   []string
	interrupted bool
	err         error
}

// speaker streams one AssistantTurn's text through its segmenter and segment player.
type speaker struct {
	player *segmentPlayer
	texts  <-chan string
	ctx    context.Context
	done   chan speechOutcome
}

func (a *Agent) startSpeaker(ctx context.Context, call AudioSession, pb *playbackState,
	control *controlRouter, texts <-chan string) *speaker {
	sp := &speaker{
		player: newSegmentPlayer(a, call, pb, control),
		texts:  texts,
		ctx:    ctx,
		done:   make(chan speechOutcome, 1),
	}
	go sp.run()
	return sp
}

func (s *speaker) run() {
	var seg segmenter
	for {
		select {
		case delta, open := <-s.texts:
			if !open {
				if tail := seg.flush(); tail != "" {
					if err := s.emit(tail); err != nil {
						s.finish(err, false)
						return
					}
				}
				s.finish(nil, false)
				return
			}
			for _, text := range seg.push(delta) {
				if err := s.emit(text); err != nil {
					s.finish(err, false)
					return
				}
			}
		case <-s.ctx.Done():
			s.finish(s.ctx.Err(), true)
			return
		}
	}
}

func (s *speaker) emit(text string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	marker := s.player.pb.markers.next()
	return s.player.play(s.ctx, text, marker)
}

func (s *speaker) finish(err error, interrupted bool) {
	out := speechOutcome{
		spoke:       s.player.anySpoke(),
		confirmed:   s.player.confirmedSegments(),
		interrupted: interrupted || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded),
		err:         err,
	}
	s.done <- out
}

// speakFixed plays fixed prompt segments sequentially and returns the number of segments
// whose playback marks were confirmed, plus interruption status.
func (a *Agent) speakFixed(ctx context.Context, call AudioSession, pb *playbackState,
	control *controlRouter, segments []string) (confirmed int, err error) {
	player := newSegmentPlayer(a, call, pb, control)
	for _, text := range segments {
		if err := ctx.Err(); err != nil {
			return len(player.confirmedSegments()), err
		}
		if err := player.play(ctx, text, pb.markers.next()); err != nil {
			return len(player.confirmedSegments()), err
		}
	}
	return len(player.confirmedSegments()), nil
}
