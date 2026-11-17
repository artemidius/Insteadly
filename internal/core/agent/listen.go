package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

const (
	// audioFeedBuffer bounds frames between the audio pump and one utterance worker. It
	// is about 1.2 s, so a normal STT handshake never stalls inbound consumption.
	audioFeedBuffer = 64
	// maxLiveWorkers bounds one active utterance plus one closing/final worker.
	maxLiveWorkers = 2
	// listenResultBuffer allows one final user utterance to be pending.
	listenResultBuffer = 1
	// listenModeBuffer is a one-slot signal: a pending barge is never duplicated.
	listenModeBuffer = 1
	dtmfRingSize     = 16
	markBufferSize   = 64
)

type listenerMode int32

const (
	modeListening listenerMode = iota
	modeAssistant
	modeTools
	modeHolding
	modeTerminal
)

// utteranceResult is one closed STT utterance. Discarded utterances never reach the
// conversation loop.
type utteranceResult struct {
	text string
}

type workerResult struct {
	discard bool
	text    string
	err     error
}

// utteranceWorker owns exactly one STT stream for one VAD-delimited utterance. It opens
// the stream itself so a network handshake never blocks the audio pump, drains cumulative
// partials concurrently, treats Close as end-of-input, and requires exactly one final.
type utteranceWorker struct {
	stt      ports.SpeechToText
	language string
	hints    []string
	cleanup  time.Duration
	feed     chan []byte
	end      chan struct{}
	done     chan<- workerResult
	discard  atomic.Bool
	endOnce  sync.Once
}

func newUtteranceWorker(stt ports.SpeechToText, language string, hints []string, cleanup time.Duration, done chan<- workerResult) *utteranceWorker {
	return &utteranceWorker{
		stt:      stt,
		language: language,
		hints:    hints,
		cleanup:  cleanup,
		feed:     make(chan []byte, audioFeedBuffer),
		end:      make(chan struct{}),
		done:     done,
	}
}

// closeInput signals end-of-input exactly once.
func (w *utteranceWorker) closeInput() {
	w.endOnce.Do(func() { close(w.end) })
}

func (w *utteranceWorker) run(ctx context.Context) {
	stream, err := w.stt.TranscribeStream(ctx, ports.StreamOptions{Language: w.language, Hints: w.hints})
	if err != nil {
		w.report(workerResult{discard: w.discard.Load(), err: fmt.Errorf("stt open: %w", err)})
		return
	}

	type drained struct {
		finals   []string
		partials int
	}
	drainedCh := make(chan drained, 1)
	resultsDone := make(chan struct{})
	go func() {
		defer close(resultsDone)
		var r drained
		for ev := range stream.Results() {
			if ev.Final {
				r.finals = append(r.finals, ev.Text)
			} else {
				r.partials++
			}
		}
		drainedCh <- r
	}()

	var feedErr error
feed:
	for {
		select {
		case frame, open := <-w.feed:
			if !open {
				break feed
			}
			if err := stream.Feed(ctx, domain.AudioChunk{Data: frame}); err != nil {
				feedErr = fmt.Errorf("stt feed: %w", err)
				break feed
			}
		case <-w.end:
			break feed
		case <-ctx.Done():
			feedErr = ctx.Err()
			break feed
		}
	}

	// Close is mandatory: it is the STT end-of-input signal. A fresh bounded context so a
	// canceled call cannot skip the final drain.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cleanup)
	defer cancel()
	closeErr := stream.Close(cleanupCtx)

	var res drained
	select {
	case res = <-drainedCh:
	case <-cleanupCtx.Done():
	}
	if feedErr != nil {
		w.report(workerResult{discard: w.discard.Load(), err: errors.Join(feedErr, closeErr)})
		return
	}
	if closeErr != nil {
		w.report(workerResult{discard: w.discard.Load(), err: fmt.Errorf("stt close: %w", closeErr)})
		return
	}
	if len(res.finals) == 0 {
		w.report(workerResult{discard: w.discard.Load(), err: errors.New("stt: results closed without a final transcript")})
		return
	}
	if len(res.finals) > 1 {
		w.report(workerResult{discard: w.discard.Load(), err: fmt.Errorf("stt: multiple final transcripts (%d)", len(res.finals))})
		return
	}
	w.report(workerResult{discard: w.discard.Load(), text: res.finals[0]})
}

func (w *utteranceWorker) report(r workerResult) {
	select {
	case w.done <- r:
	default:
		// The listener's result queue is full. Its pending-final bound is the contract;
		// the overflow is reported by the listener as a conversation error.
	}
}

// listener is the single owner of inbound audio framing, VAD, hold classification, and
// per-utterance STT workers. It publishes only conversation observations to the agent:
// final transcripts, barge/hold signals, and operational errors.
type listener struct {
	agent *Agent
	call  AudioSession
	hints []string

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	mode      listenerMode
	capturing bool
	holding   bool
	stopOnce  sync.Once

	results     chan utteranceResult
	barges      chan struct{}
	holdEvents  chan struct{}
	holdSpeech  chan struct{}
	holdSilence chan struct{}
	errs        chan error
	workerDone  chan workerResult

	workerWG sync.WaitGroup
	done     chan struct{}

	vad            *vad
	holdMu         sync.Mutex
	hold           *holdDetector
	tail           []byte
	active         *utteranceWorker
	liveWorkers    int
	closingWorkers int
	holdSilenceRun int
	fatal          error
}

func newListener(a *Agent, call AudioSession, hints []string) *listener {
	ctx, cancel := context.WithCancel(context.Background())
	return &listener{
		agent:       a,
		call:        call,
		hints:       hints,
		ctx:         ctx,
		cancel:      cancel,
		results:     make(chan utteranceResult, listenResultBuffer),
		barges:      make(chan struct{}, listenModeBuffer),
		holdEvents:  make(chan struct{}, listenModeBuffer),
		holdSpeech:  make(chan struct{}, listenModeBuffer),
		holdSilence: make(chan struct{}, listenModeBuffer),
		errs:        make(chan error, 1),
		workerDone:  make(chan workerResult, maxLiveWorkers),
		done:        make(chan struct{}),
		vad:         newVAD(a.cfg.VAD),
		hold:        newHoldDetector(a.cfg.Hold),
	}
}

func (l *listener) start() {
	go l.run()
}

// stop cancels the pump and joins every worker before the agent returns.
func (l *listener) stop(cleanupCtx context.Context) {
	l.stopOnce.Do(func() {
		l.cancel()
		<-l.done
	})
	waited := make(chan struct{})
	go func() {
		l.workerWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-cleanupCtx.Done():
	}
}

// setMode publishes the agent's conversation state. Enabling assistant mode while the
// user is already being transcribed is a real barge: pending speech must interrupt the new
// assistant turn instead of being spoken over.
func (l *listener) setMode(m listenerMode) {
	l.mu.Lock()
	prev := l.mode
	l.mode = m
	capturing := l.capturing
	if m == modeListening && prev == modeHolding {
		l.holding = false
		l.holdSilenceRun = 0
	}
	l.mu.Unlock()
	if m == modeListening && prev == modeHolding {
		l.holdMu.Lock()
		l.hold.reset()
		l.holdMu.Unlock()
	}
	if m == modeAssistant && prev != modeAssistant && capturing {
		l.signal(l.barges)
	}
}

func (l *listener) currentMode() listenerMode {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

// drainBarge discards a stale barge notification. The final transcript remains in the
// results channel, so clearing the notification never loses the user utterance.
func (l *listener) drainBarge() {
	for {
		select {
		case <-l.barges:
		default:
			return
		}
	}
}

func (l *listener) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (l *listener) resultsChannel() <-chan utteranceResult { return l.results }
func (l *listener) bargeChannel() <-chan struct{}          { return l.barges }
func (l *listener) holdChannel() <-chan struct{}           { return l.holdEvents }
func (l *listener) holdSpeechChannel() <-chan struct{}     { return l.holdSpeech }
func (l *listener) holdSilenceChannel() <-chan struct{}    { return l.holdSilence }
func (l *listener) errorChannel() <-chan error             { return l.errs }

// takePending consumes one already-final user utterance, if any. The agent uses it before
// starting assistant playback so speech captured during tool execution interrupts it.
func (l *listener) takePending() (utteranceResult, bool) {
	select {
	case r := <-l.results:
		return r, true
	default:
		return utteranceResult{}, false
	}
}

func (l *listener) run() {
	defer close(l.done)
	for {
		select {
		case wr := <-l.workerDone:
			l.handleWorkerDone(wr)
			continue
		default:
		}
		select {
		case <-l.ctx.Done():
			return
		case wr := <-l.workerDone:
			l.handleWorkerDone(wr)
		case chunk, open := <-l.call.Inbound():
			if !open {
				l.fatalError(errInboundClosed)
				return
			}
			if err := l.consume(chunk.Data); err != nil {
				l.fatalError(err)
				return
			}
		}
	}
}

// consume reframes one arbitrary canonical chunk and processes every complete frame.
func (l *listener) consume(data []byte) error {
	if len(data) == 0 || len(data)%2 != 0 {
		return fmt.Errorf("agent: inbound chunk must be non-empty even-length PCM16 (got %d bytes)", len(data))
	}
	l.tail = append(l.tail, data...)
	for len(l.tail) >= domain.FrameBytes {
		frame := make([]byte, domain.FrameBytes)
		copy(frame, l.tail[:domain.FrameBytes])
		l.tail = l.tail[domain.FrameBytes:]
		if err := l.handleFrame(frame); err != nil {
			return err
		}
	}
	if len(l.tail) == 0 {
		l.tail = nil
	}
	return nil
}

func (l *listener) handleFrame(frame []byte) error {
	mode := l.currentMode()
	if mode == modeTerminal {
		return nil
	}
	l.mu.Lock()
	holding := l.holding
	l.mu.Unlock()

	l.holdMu.Lock()
	holdCandidate := l.hold.candidate()
	l.holdMu.Unlock()
	freeze := mode == modeAssistant || holding || holdCandidate
	vadMode := vadListen
	if mode == modeAssistant {
		vadMode = vadBarge
	}
	res, err := l.vad.push(frame, vadMode, freeze)
	if err != nil {
		return err
	}

	if mode == modeHolding || holding {
		return l.handleHoldingFrame(res)
	}

	// Hold classification runs only while no user utterance is being transcribed.
	noUtterance := l.active == nil && !l.vad.inUtterance && l.closingWorkers == 0
	if noUtterance && (mode == modeListening || mode == modeTools || mode == modeAssistant) {
		l.holdMu.Lock()
		_, confirmed := l.hold.observe(res.Features, res.Speech, res.Active)
		l.holdMu.Unlock()
		if confirmed {
			l.mu.Lock()
			l.holding = true
			l.holdSilenceRun = 0
			l.mu.Unlock()
			l.resetUtteranceState()
			l.signal(l.holdEvents)
			return nil
		}
	}

	if res.Onset {
		l.mu.Lock()
		l.capturing = true
		l.mu.Unlock()
		if mode == modeAssistant {
			l.signal(l.barges)
		}
		if err := l.startWorker(res.Forward); err != nil {
			return err
		}
		return nil
	}

	// While an utterance is open, forward the live frame (including the final hangover
	// frame, which sets End and resets the VAD state) before signalling end-of-input.
	if len(res.Forward) > 0 {
		if l.active == nil {
			return errUtteranceOverflow
		}
		for _, f := range res.Forward {
			select {
			case l.active.feed <- f:
			default:
				return errUtteranceOverflow
			}
		}
	}
	if res.End {
		if l.active != nil {
			if res.Discard {
				l.active.discard.Store(true)
			}
			l.active.closeInput()
			l.active = nil
			l.closingWorkers++
			l.mu.Lock()
			l.capturing = false
			l.mu.Unlock()
		}
	}
	return nil
}

func (l *listener) handleHoldingFrame(res vadResult) error {
	if res.Onset {
		l.mu.Lock()
		l.holding = false
		l.capturing = true
		l.holdSilenceRun = 0
		l.mu.Unlock()
		l.hold.reset()
		l.signal(l.holdSpeech)
		return l.startWorker(res.Forward)
	}
	if len(res.Forward) > 0 && l.vad.inUtterance {
		// A speech exit started an utterance; hold is no longer active.
		if l.active == nil {
			return errUtteranceOverflow
		}
		for _, f := range res.Forward {
			select {
			case l.active.feed <- f:
			default:
				return errUtteranceOverflow
			}
		}
	}
	if res.End && l.active != nil {
		l.active.closeInput()
		l.active = nil
		l.closingWorkers++
		l.mu.Lock()
		l.capturing = false
		l.mu.Unlock()
	}
	if !res.Active {
		l.mu.Lock()
		l.holdSilenceRun++
		exited := l.holdSilenceRun >= l.agent.cfg.Hold.ExitSilenceFrames
		if exited {
			l.holding = false
			l.holdSilenceRun = 0
		}
		l.mu.Unlock()
		if exited {
			l.hold.reset()
			l.signal(l.holdSilence)
		}
		return nil
	}
	l.mu.Lock()
	l.holdSilenceRun = 0
	l.mu.Unlock()
	return nil
}

// resetUtteranceState drops any half-open VAD state after a confirmed hold so hold audio
// never becomes a transcript.
func (l *listener) resetUtteranceState() {
	l.vad.resetUtterance()
	if l.active != nil {
		l.active.discard.Store(true)
		l.active.closeInput()
		l.active = nil
		l.closingWorkers++
		l.mu.Lock()
		l.capturing = false
		l.mu.Unlock()
	}
	l.holdMu.Lock()
	l.hold = newHoldDetector(l.agent.cfg.Hold)
	l.holdMu.Unlock()
}

func (l *listener) startWorker(preRoll [][]byte) error {
	if l.liveWorkers >= maxLiveWorkers {
		return errUtteranceOverflow
	}
	w := newUtteranceWorker(l.agent.cfg.STT, sttLanguage, l.hints, l.agent.cfg.CleanupTimeout, l.workerDone)
	l.liveWorkers++
	l.active = w
	for _, frame := range preRoll {
		select {
		case w.feed <- frame:
		default:
			return errUtteranceOverflow
		}
	}
	l.workerWG.Add(1)
	go func() {
		defer l.workerWG.Done()
		w.run(l.ctx)
	}()
	return nil
}

func (l *listener) handleWorkerDone(r workerResult) {
	l.liveWorkers--
	if l.closingWorkers > 0 {
		l.closingWorkers--
	}
	if r.err != nil {
		l.fatalError(r.err)
		return
	}
	if r.discard {
		return
	}
	select {
	case l.results <- utteranceResult{text: r.text}:
	default:
		l.fatalError(errUtteranceOverflow)
	}
}

func (l *listener) fatalError(err error) {
	if l.fatal == nil {
		l.fatal = err
	}
	select {
	case l.errs <- err:
	default:
	}
	l.cancel()
}
