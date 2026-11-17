package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// AudioSession is the narrow view of a live call the agent needs. It cannot consume the
// runner's event channel or hang up.
type AudioSession interface {
	Inbound() <-chan domain.AudioChunk
	SendAudio(context.Context, domain.AudioChunk) error
	SendDTMF(context.Context, string) error
	ClearAudio(context.Context) error
	MarkPlayback(context.Context, string) error
}

// Engine runs the answered-call audio/dialogue loop.
type Engine interface {
	// Run starts after EventAnswered and returns only after a validated dialogue outcome,
	// call end, cancellation, or an operational/conversation-limit error. ended is owned
	// by the runner and yields the terminal CallEvent before closing; control is the
	// bounded post-answer DTMF/playback-mark relay and is closed at call end or on
	// relay overflow.
	Run(ctx context.Context, order domain.Order, call AudioSession,
		ended <-chan domain.CallEvent, control <-chan domain.CallEvent) (domain.Outcome, error)
}

// Defaults are reviewable core constants, not env surface; only tests override them
// through Config. CleanupTimeout must exceed the C2 STT (15 s) and realtime TTS (30 s)
// final-drain limits because C4 closes one TTS stream per bounded segment.
const (
	defaultListenTimeout        = 20 * time.Second
	defaultMaxUserTurns         = 12
	defaultMaxToolRounds        = 8
	defaultMaxCallDuration      = 5 * time.Minute
	defaultCleanupTimeout       = 35 * time.Second
	defaultAssistantTurnTimeout = 45 * time.Second
	defaultPlaybackAckTimeout   = 30 * time.Second
	defaultInterruptionTimeout  = 2 * time.Second
	defaultMaxOutcomeRecovery   = 2
	defaultMaxIVRSteps          = 4
	defaultIVRWaitTimeout       = 15 * time.Second
)

// Config configures one stateless agent service. Zero fields use the defaults above;
// negative or impossible values are rejected. These are constructor/test knobs and are
// never serialized into health/config summaries.
type Config struct {
	STT      ports.SpeechToText
	TTS      ports.TextToSpeech
	Dialogue ports.Dialogue
	Orders   ports.OrderStore
	Logger   *slog.Logger

	// NewTools creates the per-call tool executor; nil selects the production booking
	// executor backed by Orders.
	NewTools ToolFactory

	// Now is the clock shared by the prompt, tool validation, IVR policy, and tests.
	Now func() time.Time

	// VAD and Hold group the superseded M0 gate knobs.
	VAD  VADConfig
	Hold HoldConfig

	ListenTimeout        time.Duration
	MaxUserTurns         int
	MaxToolRounds        int
	MaxCallDuration      time.Duration
	CleanupTimeout       time.Duration
	AssistantTurnTimeout time.Duration
	PlaybackAckTimeout   time.Duration
	InterruptionTimeout  time.Duration
	MaxOutcomeRecovery   int
	MaxIVRSteps          int
	IVRWaitTimeout       time.Duration
}

// Agent is a stateless service: all per-call state lives in Run.
type Agent struct {
	cfg Config
	log *slog.Logger
}

var _ Engine = (*Agent)(nil)

// New validates the config, applies defaults, and performs no network I/O.
func New(cfg Config) (*Agent, error) {
	if cfg.STT == nil || cfg.TTS == nil || cfg.Dialogue == nil || cfg.Orders == nil {
		return nil, errors.New("agent: stt, tts, dialogue, and orders are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := applyDefaults(&cfg); err != nil {
		return nil, err
	}
	return &Agent{cfg: cfg, log: cfg.Logger}, nil
}

func applyDefaults(cfg *Config) error {
	if err := applyVADDefaults(&cfg.VAD); err != nil {
		return err
	}
	if err := applyHoldDefaults(&cfg.Hold); err != nil {
		return err
	}
	if err := nonNegativeDuration("listen timeout", cfg.ListenTimeout); err != nil {
		return err
	}
	if cfg.ListenTimeout == 0 {
		cfg.ListenTimeout = defaultListenTimeout
	}
	if err := nonNegative("maximum user turns", cfg.MaxUserTurns); err != nil {
		return err
	}
	if cfg.MaxUserTurns == 0 {
		cfg.MaxUserTurns = defaultMaxUserTurns
	}
	if err := nonNegative("maximum tool rounds", cfg.MaxToolRounds); err != nil {
		return err
	}
	if cfg.MaxToolRounds == 0 {
		cfg.MaxToolRounds = defaultMaxToolRounds
	}
	if err := nonNegativeDuration("maximum call duration", cfg.MaxCallDuration); err != nil {
		return err
	}
	if cfg.MaxCallDuration == 0 {
		cfg.MaxCallDuration = defaultMaxCallDuration
	}
	if err := nonNegativeDuration("cleanup timeout", cfg.CleanupTimeout); err != nil {
		return err
	}
	if cfg.CleanupTimeout == 0 {
		cfg.CleanupTimeout = defaultCleanupTimeout
	}
	if err := nonNegativeDuration("assistant turn timeout", cfg.AssistantTurnTimeout); err != nil {
		return err
	}
	if cfg.AssistantTurnTimeout == 0 {
		cfg.AssistantTurnTimeout = defaultAssistantTurnTimeout
	}
	if err := nonNegativeDuration("playback acknowledgement timeout", cfg.PlaybackAckTimeout); err != nil {
		return err
	}
	if cfg.PlaybackAckTimeout == 0 {
		cfg.PlaybackAckTimeout = defaultPlaybackAckTimeout
	}
	if err := nonNegativeDuration("interruption timeout", cfg.InterruptionTimeout); err != nil {
		return err
	}
	if cfg.InterruptionTimeout == 0 {
		cfg.InterruptionTimeout = defaultInterruptionTimeout
	}
	if err := nonNegative("maximum outcome recovery", cfg.MaxOutcomeRecovery); err != nil {
		return err
	}
	if cfg.MaxOutcomeRecovery == 0 {
		cfg.MaxOutcomeRecovery = defaultMaxOutcomeRecovery
	}
	if err := nonNegative("maximum ivr steps", cfg.MaxIVRSteps); err != nil {
		return err
	}
	if cfg.MaxIVRSteps == 0 {
		cfg.MaxIVRSteps = defaultMaxIVRSteps
	}
	if err := nonNegativeDuration("ivr wait timeout", cfg.IVRWaitTimeout); err != nil {
		return err
	}
	if cfg.IVRWaitTimeout == 0 {
		cfg.IVRWaitTimeout = defaultIVRWaitTimeout
	}
	if cfg.CleanupTimeout < cfg.InterruptionTimeout {
		return errors.New("agent: cleanup timeout must not be shorter than the interruption timeout")
	}
	return nil
}

func nonNegative(name string, v int) error {
	if v < 0 {
		return fmt.Errorf("agent: %s must not be negative", name)
	}
	return nil
}

func nonNegativeDuration(name string, v time.Duration) error {
	if v < 0 {
		return fmt.Errorf("agent: %s must not be negative", name)
	}
	return nil
}

// callState is all per-call conversation state.
type callState struct {
	order   domain.Order
	now     time.Time
	tools   ToolExecutor
	session ports.DialogueSession
	control *controlRouter
	pb      *playbackState

	userTurns       int
	emptyUtterances int
	recoveries      recoveryBudget
	ivr             ivrState
	greeting        greetingState
}

// Run executes the answered-call loop for one order. All per-call state is local.
func (a *Agent) Run(ctx context.Context, order domain.Order, call AudioSession,
	ended <-chan domain.CallEvent, control <-chan domain.CallEvent) (final domain.Outcome, runErr error) {
	if call == nil {
		return domain.Outcome{}, errors.New("agent: nil audio session")
	}
	if strings.TrimSpace(string(order.ID)) == "" {
		return domain.Outcome{}, errors.New("agent: order id is required")
	}
	if err := ctx.Err(); err != nil {
		return domain.Outcome{}, err
	}

	callCtx, cancelCall := context.WithTimeout(ctx, a.cfg.MaxCallDuration)
	defer cancelCall()

	var endedSeen atomic.Bool
	// Normalize the internal terminal sentinel back to the actual cause: a real terminal
	// call event keeps ErrCallEnded, caller cancellation keeps context.Canceled, and the
	// runner/agent deadline keeps context.DeadlineExceeded. The latter is then classified as
	// a conversation limit when the caller's context is still live.
	defer func() {
		if errors.Is(runErr, ErrCallEnded) {
			switch {
			case endedSeen.Load():
			case ctx.Err() != nil:
				runErr = ctx.Err()
			case callCtx.Err() != nil:
				runErr = callCtx.Err()
			}
		}
		if runErr == nil || errors.Is(runErr, ErrCallEnded) {
			return
		}
		if callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			runErr = markLimit(runErr)
		}
	}()

	runCtx, cancelRun := context.WithCancel(callCtx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ended:
			endedSeen.Store(true)
		case <-runCtx.Done():
		}
		cancelRun()
	}()
	defer func() {
		cancelRun()
		<-watcherDone
	}()

	ctrl := newControlRouter(control)
	ctrl.start()
	defer ctrl.stopRouter()

	st := &callState{
		order:   order,
		now:     a.cfg.Now(),
		control: ctrl,
		pb:      &playbackState{},
	}
	st.tools = a.newToolExecutor(order, st.now)

	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(runCtx), a.cfg.CleanupTimeout)
	defer cancelCleanup()

	hints := make([]string, 0, 1)
	if name := strings.TrimSpace(order.Clinic.Name); name != "" {
		hints = append(hints, name)
	}
	l := newListener(a, call, hints)
	l.start()
	// Worker cancellation/join happens before the dialogue session close below.
	defer l.stop(cleanupCtx)
	defer func() {
		if st.session != nil {
			a.closeDialogue(cleanupCtx, st.session, order.ID)
		}
	}()

	greet, err := a.greet(runCtx, call, l, st)
	if err != nil {
		return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
	}
	if greet.hold {
		if outcome, err := a.holdLoop(runCtx, call, l, st); err != nil || outcome != nil {
			if err != nil {
				return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
			}
			return *outcome, nil
		}
	}
	return a.conversationLoop(runCtx, call, l, st, greet.pending)
}

// greet plays the fixed AI-disclosure and purpose segments, tracking each confirmed
// segment separately so the dialogue prompt can require any missing disclosure on the
// first real assistant response.
func (a *Agent) greet(ctx context.Context, call AudioSession, l *listener, st *callState) (fixedTurn, error) {
	segments := greetingSegments(st.order)
	res := a.speakFixedInterruptible(ctx, call, l, st, segments)
	if res.err != nil {
		return fixedTurn{}, res.err
	}
	st.greeting.Disclosure = res.confirmed >= 1
	st.greeting.Purpose = res.confirmed >= 2
	if res.interruption != nil {
		kind := res.interruption.kind
		if kind == interruptHold {
			return fixedTurn{hold: true}, nil
		}
		return fixedTurn{pending: res.interruption.pending}, nil
	}
	return fixedTurn{}, nil
}

// conversationLoop is the single listen/act state machine. It never returns on a
// recoverable model failure; recovery is bounded inside driveTurn.
func (a *Agent) conversationLoop(ctx context.Context, call AudioSession, l *listener,
	st *callState, pending *utteranceResult) (domain.Outcome, error) {
	listenTimer := time.NewTimer(a.cfg.ListenTimeout)
	defer listenTimer.Stop()
	var ivrTimer *time.Timer
	defer func() {
		if ivrTimer != nil {
			ivrTimer.Stop()
		}
	}()

	for {
		if st.userTurns >= a.cfg.MaxUserTurns {
			return domain.Outcome{}, a.mapError(false, errUserTurnsExceeded)
		}
		l.setMode(modeListening)
		l.drainBarge()
		resetTimer(listenTimer, a.cfg.ListenTimeout)
		ivrCh := resetIVRTimer(&ivrTimer, st, a.cfg)

		if pending == nil {
			select {
			case r := <-l.resultsChannel():
				pending = &r
			case <-l.holdChannel():
				if outcome, err := a.holdLoop(ctx, call, l, st); err != nil || outcome != nil {
					return outcomeValue(outcome), err
				}
				continue
			case err := <-l.errorChannel():
				return domain.Outcome{}, a.mapError(false, err)
			case err := <-st.control.errChannel():
				return domain.Outcome{}, a.mapError(false, err)
			case <-st.control.doneChannel():
				return domain.Outcome{}, a.mapError(ctx.Err() != nil, errControlClosed)
			case <-ivrCh:
				out := ivrDeadEndOutcome()
				return out, nil
			case <-listenTimer.C:
				return domain.Outcome{}, a.mapError(false, errListenTimeout)
			case <-ctx.Done():
				return domain.Outcome{}, ErrCallEnded
			}
		}

		res := *pending
		pending = nil
		step, err := a.handleUtterance(ctx, call, l, st, res.text)
		if err != nil {
			return domain.Outcome{}, err
		}
		switch {
		case step.outcome != nil:
			a.log.Info("agent: dialogue outcome",
				"order_id", st.order.ID,
				"outcome_kind", step.outcome.Kind,
				"fail_reason", step.outcome.FailReason,
				"user_turns", st.userTurns,
			)
			return *step.outcome, nil
		case step.hold:
			if outcome, err := a.holdLoop(ctx, call, l, st); err != nil || outcome != nil {
				return outcomeValue(outcome), err
			}
		case step.pending != nil:
			pending = step.pending
		}
	}
}

// utteranceStep is the result of one final transcript.
type utteranceStep struct {
	outcome *domain.Outcome
	hold    bool
	pending *utteranceResult
}

// handleUtterance classifies one final transcript: empty repeat, deterministic IVR menu,
// or an ordinary human dialogue turn.
func (a *Agent) handleUtterance(ctx context.Context, call AudioSession, l *listener,
	st *callState, text string) (utteranceStep, error) {
	if strings.TrimSpace(text) == "" {
		st.emptyUtterances++
		if st.emptyUtterances > 1 {
			return utteranceStep{}, a.mapError(false, errEmptyTranscript)
		}
		res := a.speakFixedInterruptible(ctx, call, l, st, []string{pleaseRepeatPrompt})
		if res.err != nil {
			return utteranceStep{}, res.err
		}
		if res.interruption != nil {
			return utteranceStep{hold: res.interruption.kind == interruptHold, pending: res.interruption.pending}, nil
		}
		return utteranceStep{}, nil
	}
	st.emptyUtterances = 0

	handled, deadEnd, err := a.handleMenuTranscript(ctx, call, st, text)
	if err != nil {
		return utteranceStep{}, err
	}
	if deadEnd != nil {
		return utteranceStep{outcome: deadEnd}, nil
	}
	if handled {
		return utteranceStep{}, nil
	}

	if st.userTurns >= a.cfg.MaxUserTurns {
		return utteranceStep{}, a.mapError(false, errUserTurnsExceeded)
	}
	st.userTurns++
	session, err := a.ensureSession(ctx, st)
	if err != nil {
		return utteranceStep{}, fmt.Errorf("dialogue session: %w", err)
	}
	firstSource := func(turnCtx context.Context) (ports.AssistantTurn, error) {
		turn, err := session.SendUserTurn(turnCtx, text)
		if err != nil {
			return nil, fmt.Errorf("dialogue user turn: %w", err)
		}
		return turn, nil
	}
	return a.driveTurn(ctx, call, l, st, session, firstSource)
}

func (a *Agent) ensureSession(ctx context.Context, st *callState) (ports.DialogueSession, error) {
	if st.session != nil {
		return st.session, nil
	}
	session, err := a.cfg.Dialogue.NewSession(ctx, ports.SessionRequest{
		OrderID:       st.order.ID,
		SystemPrompt:  buildSystemPrompt(st.order, st.now, st.greeting),
		Tools:         ToolDefinitions(),
		OutcomeSchema: OutcomeSchema(),
	})
	if err != nil {
		return nil, err
	}
	st.session = session
	return session, nil
}

// turnSource starts the next assistant turn. Recovery, tool continuation, and ordinary
// user turns all use the same streaming playback/validation path.
type turnSource func(ctx context.Context) (ports.AssistantTurn, error)

// driveTurn is the shared turn runner: always drain text, reconcile playback, await the
// result, execute ordinary tools at most once, validate every outcome, and bound recovery.
func (a *Agent) driveTurn(ctx context.Context, call AudioSession, l *listener,
	st *callState, session ports.DialogueSession, source turnSource) (utteranceStep, error) {
	toolRounds := 0
	for {
		turnCtx, cancelTurn := context.WithCancel(ctx)
		turn, err := source(turnCtx)
		if err != nil {
			cancelTurn()
			if isTerminalTurnError(ctx, err) {
				return utteranceStep{}, ErrCallEnded
			}
			return utteranceStep{}, err
		}

		// Speech captured during tool execution must interrupt the next assistant turn
		// before it speaks; the already-submitted tool results stay committed.
		if pending, ok := l.takePending(); ok {
			cancelTurn()
			a.waitTurnRollback(ctx, turn)
			if err := a.reconcileInterruptedTurn(ctx, session, turn, ""); err != nil {
				return utteranceStep{}, err
			}
			return utteranceStep{pending: &pending}, nil
		}

		result, speech, status, inter, err := a.playAndWait(ctx, turnCtx, call, l, st, session, turn, cancelTurn)
		cancelTurn()
		switch status {
		case turnInterrupted:
			return utteranceStep{pending: inter.pending}, nil
		case turnHeld:
			return utteranceStep{hold: true}, nil
		case turnStalled:
			next, err := a.recoverSource(ctx, session, st, recoveryStall, turn, speech)
			if err != nil {
				return utteranceStep{}, err
			}
			source = next
			continue
		case turnInvalid:
			next, err := a.recoverSource(ctx, session, st, recoveryInvalidOutcome, turn, speech)
			if err != nil {
				return utteranceStep{}, err
			}
			source = next
			continue
		}
		if err != nil {
			return utteranceStep{}, err
		}

		if result.Outcome != nil {
			if len(result.ToolCalls) > 0 {
				return utteranceStep{}, errors.New("agent: outcome turn unexpectedly contains tool calls")
			}
			if verr := st.tools.ValidateOutcome(result.Outcome); verr != nil {
				next, rerr := a.recoverSource(ctx, session, st, recoveryToolConsistency, turn, speech)
				if rerr != nil {
					return utteranceStep{}, rerr
				}
				source = next
				continue
			}
			return utteranceStep{outcome: result.Outcome}, nil
		}

		if len(result.ToolCalls) > 0 {
			if toolRounds >= a.cfg.MaxToolRounds {
				return utteranceStep{}, a.mapError(false, errToolRoundsExceeded)
			}
			toolRounds++
			results, terr := a.executeTools(ctx, st.tools, result.ToolCalls)
			if terr != nil {
				return utteranceStep{}, terr
			}
			submitted := results
			source = func(turnCtx context.Context) (ports.AssistantTurn, error) {
				next, serr := session.SubmitToolResults(turnCtx, submitted)
				if serr != nil {
					return nil, fmt.Errorf("dialogue tool results: %w", serr)
				}
				return next, nil
			}
			continue
		}

		if speech.spoke {
			return utteranceStep{}, nil
		}

		next, rerr := a.recoverSource(ctx, session, st, recoveryEmptyTurn, turn, speech)
		if rerr != nil {
			return utteranceStep{}, rerr
		}
		source = next
	}
}

// recoverSource consumes one per-call recovery round and returns a control-turn source.
func (a *Agent) recoverSource(ctx context.Context, session ports.DialogueSession, st *callState,
	reason recoveryReason, turn ports.AssistantTurn, speech speechOutcome) (turnSource, error) {
	if err := st.recoveries.next(a.cfg.MaxOutcomeRecovery); err != nil {
		return nil, a.mapError(false, err)
	}
	// Roll the failed/empty/finished assistant part back to its triggering user/tool
	// history and keep only provider-confirmed speech.
	if recErr := a.reconcileInterruptedTurn(ctx, session, turn, speech.confirmedText()); recErr != nil {
		return nil, recErr
	}
	a.log.Info("agent: outcome recovery", "reason", reason.code(), "used", st.recoveries.used)
	return func(turnCtx context.Context) (ports.AssistantTurn, error) {
		return a.recoverOutcome(turnCtx, session, reason)
	}, nil
}

// turnStatus is the disposition of one assistant turn's playback + result wait.
type turnStatus int

const (
	turnCompleted turnStatus = iota
	turnInterrupted
	turnHeld
	turnStalled
	turnInvalid
)

// playAndWait starts segmented playback and waits for both the streamed text and the turn
// result, honoring barge/hold/terminal races. A non-nil error with turnCompleted is the
// turn's completed result.
func (a *Agent) playAndWait(ctx, turnCtx context.Context, call AudioSession, l *listener, st *callState,
	session ports.DialogueSession, turn ports.AssistantTurn,
	cancelTurn context.CancelFunc) (ports.TurnResult, speechOutcome, turnStatus, *interruption, error) {
	l.setMode(modeAssistant)
	sp := a.startSpeaker(turnCtx, call, st.pb, st.control, turn.TextChunks())

	waitCtx, waitCancel := context.WithTimeout(turnCtx, a.cfg.AssistantTurnTimeout)
	defer waitCancel()
	type waitResult struct {
		res ports.TurnResult
		err error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		r, err := turn.Wait(waitCtx)
		waitCh <- waitResult{res: r, err: err}
	}()

	var speech speechOutcome
	var wr waitResult
	haveSpeech, haveResult := false, false
	for !haveSpeech || !haveResult {
		select {
		case speech = <-sp.done:
			haveSpeech = true
			if speech.err != nil && !errors.Is(speech.err, context.Canceled) {
				cancelTurn()
				a.drainSpeaker(sp, haveSpeech)
				return ports.TurnResult{}, speech, turnCompleted, nil, speech.err
			}
		case wr = <-waitCh:
			haveResult = true
			waitCancel()
		case <-l.bargeChannel():
			return a.interruptTurn(ctx, call, l, st, session, turn, sp, cancelTurn, haveSpeech, speech, interruptBarge)
		case <-l.holdChannel():
			return a.interruptTurn(ctx, call, l, st, session, turn, sp, cancelTurn, haveSpeech, speech, interruptHold)
		case err := <-l.errorChannel():
			cancelTurn()
			return ports.TurnResult{}, a.speakerSpeech(sp, haveSpeech, speech), turnCompleted, nil, err
		case err := <-st.control.errChannel():
			cancelTurn()
			return ports.TurnResult{}, a.speakerSpeech(sp, haveSpeech, speech), turnCompleted, nil, err
		case <-st.control.doneChannel():
			cancelTurn()
			speechOut := a.speakerSpeech(sp, haveSpeech, speech)
			if ctx.Err() != nil {
				return ports.TurnResult{}, speechOut, turnCompleted, nil, ErrCallEnded
			}
			return ports.TurnResult{}, speechOut, turnCompleted, nil, errControlClosed
		case <-ctx.Done():
			cancelTurn()
			return ports.TurnResult{}, a.speakerSpeech(sp, haveSpeech, speech), turnCompleted, nil, ErrCallEnded
		}
	}

	if wr.err != nil {
		switch {
		case errors.Is(wr.err, context.DeadlineExceeded) && ctx.Err() == nil:
			// The 45 s model-result deadline: stop any remaining playback and clean up
			// before the recovery control retries the turn.
			cancelTurn()
			st.pb.bump()
			if ierr := a.interruptPlayback(ctx, call, st.pb); ierr != nil {
				return ports.TurnResult{}, speech, turnCompleted, nil, ierr
			}
			return ports.TurnResult{}, a.speakerSpeech(sp, haveSpeech, speech), turnStalled, nil, nil
		case errors.Is(wr.err, domain.ErrInvalidOutcome):
			return ports.TurnResult{}, speech, turnInvalid, nil, nil
		case ctx.Err() != nil:
			return ports.TurnResult{}, speech, turnCompleted, nil, ErrCallEnded
		default:
			return ports.TurnResult{}, speech, turnCompleted, nil, fmt.Errorf("dialogue turn wait: %w", wr.err)
		}
	}
	return wr.res, speech, turnCompleted, nil, nil
}

// speakerSpeech returns the already-received speech outcome or joins the speaker.
func (a *Agent) speakerSpeech(sp *speaker, have bool, speech speechOutcome) speechOutcome {
	if have {
		return speech
	}
	return a.drainSpeaker(sp, false)
}

// interruptTurn performs the full interruption sequence: cancel the turn, bounded Abort +
// ClearAudio, join the speaker, reconcile only confirmed segments with dialogue history,
// and preserve any new user utterance.
func (a *Agent) interruptTurn(ctx context.Context, call AudioSession, l *listener, st *callState,
	session ports.DialogueSession, turn ports.AssistantTurn, sp *speaker,
	cancelTurn context.CancelFunc, haveSpeech bool, speech speechOutcome,
	kind interruptKind) (ports.TurnResult, speechOutcome, turnStatus, *interruption, error) {
	cancelTurn()
	st.pb.bump()
	interruptErr := a.interruptPlayback(ctx, call, st.pb)
	speech = a.speakerSpeech(sp, haveSpeech, speech)
	if interruptErr != nil {
		return ports.TurnResult{}, speech, turnCompleted, nil, interruptErr
	}
	if err := a.reconcileInterruptedTurn(ctx, session, turn, speech.confirmedText()); err != nil {
		return ports.TurnResult{}, speech, turnCompleted, nil, err
	}
	pending, havePending := l.takePending()
	inter := &interruption{kind: kind, confirmed: speech.confirmed}
	if havePending {
		inter.pending = &pending
	}
	status := turnInterrupted
	if kind == interruptHold {
		status = turnHeld
	}
	return ports.TurnResult{}, speech, status, inter, nil
}

// drainSpeaker waits for the speaker goroutine after a cancellation/Abort. It discards
// unconfirmed audio; only segments with provider marks are reported.
func (a *Agent) drainSpeaker(sp *speaker, already bool) speechOutcome {
	if already {
		return speechOutcome{}
	}
	select {
	case out := <-sp.done:
		return out
	case <-time.After(a.cfg.CleanupTimeout):
		return speechOutcome{interrupted: true}
	}
}

// speakFixedInterruptible plays fixed prompt segments while honoring barge/hold/terminal
// events. It returns how many segments were confirmed before any interruption.
type fixedResult struct {
	confirmed    int
	interruption *interruption
	err          error
}

func (a *Agent) speakFixedInterruptible(ctx context.Context, call AudioSession, l *listener,
	st *callState, segments []string) fixedResult {
	st.pb.bump()
	l.setMode(modeAssistant)
	fixedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan fixedDone, 1)
	go func() {
		n, err := a.speakFixed(fixedCtx, call, st.pb, st.control, segments)
		done <- fixedDone{confirmed: n, err: err}
	}()

	select {
	case d := <-done:
		if d.err != nil {
			return fixedResult{confirmed: d.confirmed, err: d.err}
		}
		return fixedResult{confirmed: d.confirmed}
	case <-l.bargeChannel():
		return a.interruptFixed(ctx, call, l, st, cancel, done, interruptBarge)
	case <-l.holdChannel():
		return a.interruptFixed(ctx, call, l, st, cancel, done, interruptHold)
	case err := <-l.errorChannel():
		cancel()
		<-done
		return fixedResult{err: err}
	case err := <-st.control.errChannel():
		cancel()
		<-done
		return fixedResult{err: err}
	case <-st.control.doneChannel():
		cancel()
		<-done
		if ctx.Err() != nil {
			return fixedResult{err: ErrCallEnded}
		}
		return fixedResult{err: errControlClosed}
	case <-ctx.Done():
		cancel()
		<-done
		return fixedResult{err: ErrCallEnded}
	}
}

type fixedDone struct {
	confirmed int
	err       error
}

func (a *Agent) interruptFixed(ctx context.Context, call AudioSession, l *listener, st *callState,
	cancel context.CancelFunc, done <-chan fixedDone, kind interruptKind) fixedResult {
	cancel()
	st.pb.bump()
	interruptErr := a.interruptPlayback(ctx, call, st.pb)
	d := <-done
	if interruptErr != nil {
		return fixedResult{confirmed: d.confirmed, err: interruptErr}
	}
	pending, havePending := l.takePending()
	inter := &interruption{kind: kind}
	if havePending {
		inter.pending = &pending
	}
	return fixedResult{confirmed: d.confirmed, interruption: inter}
}

// holdLoop waits in the holding state: speech resumes an ordinary utterance, 500 ms of
// silence returns to listening, and 90 s of continuous hold is failed/other.
func (a *Agent) holdLoop(ctx context.Context, call AudioSession, l *listener, st *callState) (*domain.Outcome, error) {
	l.setMode(modeHolding)
	timer := time.NewTimer(a.cfg.Hold.MaxContinuous)
	defer timer.Stop()
	for {
		select {
		case <-l.holdSpeechChannel():
			l.setMode(modeListening)
			return nil, nil
		case <-l.holdSilenceChannel():
			l.setMode(modeListening)
			return nil, nil
		case <-timer.C:
			out := holdTimeoutOutcome()
			return &out, nil
		case err := <-l.errorChannel():
			return nil, err
		case err := <-st.control.errChannel():
			return nil, err
		case <-st.control.doneChannel():
			if ctx.Err() != nil {
				return nil, ErrCallEnded
			}
			return nil, errControlClosed
		case <-ctx.Done():
			return nil, ErrCallEnded
		}
	}
}

// executeTools runs ordinary calls sequentially in model order, producing exactly one
// JSON result per call ID. finish_call is reserved and must never reach this function.
func (a *Agent) executeTools(ctx context.Context, tools ToolExecutor, calls []ports.ToolCall) ([]ports.ToolResult, error) {
	results := make([]ports.ToolResult, 0, len(calls))
	for _, tc := range calls {
		if tc.Name == ToolFinishCall {
			return nil, errors.New("agent: finish_call must be intercepted by the dialogue adapter")
		}
		result, err := tools.Execute(ctx, tc)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", tc.Name, err)
		}
		if !jsonValidObject(result) {
			return nil, fmt.Errorf("agent: tool %s returned invalid JSON", tc.Name)
		}
		results = append(results, ports.ToolResult{ID: tc.ID, Result: result})
	}
	return results, nil
}

func (a *Agent) newToolExecutor(order domain.Order, now time.Time) ToolExecutor {
	if a.cfg.NewTools != nil {
		return a.cfg.NewTools(order)
	}
	return newBookingExecutor(order, a.cfg.Orders, func() time.Time { return now })
}

func (a *Agent) closeDialogue(ctx context.Context, session ports.DialogueSession, id domain.OrderID) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.CleanupTimeout)
	defer cancel()
	if err := session.Close(cctx); err != nil {
		a.log.Debug("agent: dialogue close failed", "order_id", id)
	}
}

// mapError returns ErrCallEnded when the terminal call event won the race with an agent
// operation, and classifies agent conversation limits.
func (a *Agent) mapError(ended bool, err error) error {
	if ended {
		return ErrCallEnded
	}
	switch {
	case errors.Is(err, errListenTimeout),
		errors.Is(err, errEmptyTranscript),
		errors.Is(err, errUserTurnsExceeded),
		errors.Is(err, errToolRoundsExceeded),
		errors.Is(err, errOutcomeRecoveryExhausted):
		return markLimit(err)
	default:
		return err
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func resetIVRTimer(t **time.Timer, st *callState, cfg Config) <-chan time.Time {
	if st.ivr.waitUntil.IsZero() {
		if *t != nil {
			(*t).Stop()
		}
		return nil
	}
	d := time.Until(st.ivr.waitUntil)
	if d <= 0 {
		ch := make(chan time.Time)
		close(ch)
		return ch
	}
	if *t == nil {
		*t = time.NewTimer(d)
	} else {
		resetTimer(*t, d)
	}
	return (*t).C
}

func outcomeValue(out *domain.Outcome) domain.Outcome {
	if out == nil {
		return domain.Outcome{}
	}
	return *out
}

// fixedTurn reports what happened to a fixed prompt.
type fixedTurn struct {
	hold    bool
	pending *utteranceResult
}
