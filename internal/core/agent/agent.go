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
// runner's event channel, send DTMF, or hang up.
type AudioSession interface {
	Inbound() <-chan domain.AudioChunk
	SendAudio(context.Context, domain.AudioChunk) error
}

// Engine runs the answered-call audio/dialogue loop.
type Engine interface {
	// Run starts after EventAnswered and returns only after a validated dialogue outcome,
	// call end, cancellation, or an operational/conversation-limit error. ended is owned
	// by the runner and yields the terminal CallEvent before closing.
	Run(ctx context.Context, order domain.Order, call AudioSession,
		ended <-chan domain.CallEvent) (domain.Outcome, error)
}

// Defaults for the M0 gate and conversation limits. They are core constants, not env
// surface; only tests override them through Config.
const (
	defaultSpeechThreshold    = 500.0
	defaultPreRollFrames      = 10
	defaultStartFrames        = 3
	defaultMinActiveFrames    = 10
	defaultEndSilenceFrames   = 30
	defaultMaxUtteranceFrames = 750
	defaultListenTimeout      = 20 * time.Second
	defaultMaxUserTurns       = 12
	defaultMaxToolRounds      = 8
	defaultMaxCallDuration    = 5 * time.Minute
	defaultReplayPace         = 20 * time.Millisecond
	defaultCleanupTimeout     = 5 * time.Second
)

// Config configures one stateless agent service. Zero gate/limit values use the defaults
// above; negative values are rejected. All fields are constructor/test knobs and are
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

	// Now is the clock shared by the prompt, tool validation, and tests.
	Now func() time.Time
	// Sleep is the context-aware replay pacer; nil uses a timer. Tests inject a no-op.
	Sleep func(ctx context.Context, d time.Duration) error

	SpeechThreshold    float64
	PreRollFrames      int
	StartFrames        int
	MinActiveFrames    int
	EndSilenceFrames   int
	MaxUtteranceFrames int
	ListenTimeout      time.Duration
	MaxUserTurns       int
	MaxToolRounds      int
	MaxCallDuration    time.Duration
	ReplayPace         time.Duration
	CleanupTimeout     time.Duration
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
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
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
	if cfg.SpeechThreshold < 0 {
		return errors.New("agent: speech threshold must not be negative")
	}
	if cfg.SpeechThreshold == 0 {
		cfg.SpeechThreshold = defaultSpeechThreshold
	}
	if err := nonNegative("pre-roll frames", cfg.PreRollFrames); err != nil {
		return err
	}
	if cfg.PreRollFrames == 0 {
		cfg.PreRollFrames = defaultPreRollFrames
	}
	if err := nonNegative("start frames", cfg.StartFrames); err != nil {
		return err
	}
	if cfg.StartFrames == 0 {
		cfg.StartFrames = defaultStartFrames
	}
	if err := nonNegative("minimum active frames", cfg.MinActiveFrames); err != nil {
		return err
	}
	if cfg.MinActiveFrames == 0 {
		cfg.MinActiveFrames = defaultMinActiveFrames
	}
	if err := nonNegative("end-silence frames", cfg.EndSilenceFrames); err != nil {
		return err
	}
	if cfg.EndSilenceFrames == 0 {
		cfg.EndSilenceFrames = defaultEndSilenceFrames
	}
	if err := nonNegative("maximum utterance frames", cfg.MaxUtteranceFrames); err != nil {
		return err
	}
	if cfg.MaxUtteranceFrames == 0 {
		cfg.MaxUtteranceFrames = defaultMaxUtteranceFrames
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
	if err := nonNegativeDuration("replay pace", cfg.ReplayPace); err != nil {
		return err
	}
	if cfg.ReplayPace == 0 {
		cfg.ReplayPace = defaultReplayPace
	}
	if err := nonNegativeDuration("cleanup timeout", cfg.CleanupTimeout); err != nil {
		return err
	}
	if cfg.CleanupTimeout == 0 {
		cfg.CleanupTimeout = defaultCleanupTimeout
	}

	if cfg.StartFrames > cfg.PreRollFrames {
		return errors.New("agent: start frames must not exceed pre-roll frames")
	}
	if cfg.StartFrames > cfg.MinActiveFrames {
		return errors.New("agent: start frames must not exceed minimum active frames")
	}
	if cfg.MinActiveFrames > cfg.MaxUtteranceFrames {
		return errors.New("agent: minimum active frames must not exceed maximum utterance frames")
	}
	if cfg.EndSilenceFrames > cfg.MaxUtteranceFrames {
		return errors.New("agent: end-silence frames must not exceed maximum utterance frames")
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

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run executes the answered-call loop for one order. All per-call state is local.
func (a *Agent) Run(ctx context.Context, order domain.Order, call AudioSession, ended <-chan domain.CallEvent) (final domain.Outcome, runErr error) {
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
	// The agent's own maximum call duration is a conversation limit, not a provider
	// failure. Only classify it when the caller's context is still live; a runner
	// deadline or external cancellation keeps its own meaning.
	defer func() {
		if runErr == nil || errors.Is(runErr, ErrCallEnded) {
			return
		}
		if callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			runErr = markLimit(runErr)
		}
	}()

	runCtx, cancelRun := context.WithCancel(callCtx)
	var endedSeen atomic.Bool
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

	now := a.cfg.Now()
	g := newGate(gateConfig{
		speechThreshold:    a.cfg.SpeechThreshold,
		preRollFrames:      a.cfg.PreRollFrames,
		startFrames:        a.cfg.StartFrames,
		minActiveFrames:    a.cfg.MinActiveFrames,
		endSilenceFrames:   a.cfg.EndSilenceFrames,
		maxUtteranceFrames: a.cfg.MaxUtteranceFrames,
		listenTimeout:      a.cfg.ListenTimeout,
	})
	tools := a.newToolExecutor(order, now)

	session, err := a.cfg.Dialogue.NewSession(runCtx, ports.SessionRequest{
		OrderID:       order.ID,
		SystemPrompt:  buildSystemPrompt(order, now),
		Tools:         ToolDefinitions(),
		OutcomeSchema: OutcomeSchema(),
	})
	if err != nil {
		return domain.Outcome{}, a.mapError(endedSeen.Load(), fmt.Errorf("dialogue session: %w", err))
	}
	defer a.closeDialogue(ctx, session, order.ID)

	if _, err := a.speak(runCtx, call, oneText(greeting(order))); err != nil {
		return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
	}

	hints := make([]string, 0, 1)
	if name := strings.TrimSpace(order.Clinic.Name); name != "" {
		hints = append(hints, name)
	}

	userTurns := 0
	emptyUtterances := 0
	for {
		if userTurns >= a.cfg.MaxUserTurns {
			return domain.Outcome{}, a.mapError(endedSeen.Load(), errUserTurnsExceeded)
		}

		uttered, err := g.next(runCtx, call.Inbound(), ended)
		if err != nil {
			return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
		}

		text, err := a.transcribe(runCtx, uttered.frames, hints)
		if err != nil {
			return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
		}
		if strings.TrimSpace(text) == "" {
			emptyUtterances++
			if emptyUtterances == 1 {
				if _, err := a.speak(runCtx, call, oneText(pleaseRepeatPrompt)); err != nil {
					return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
				}
				continue
			}
			return domain.Outcome{}, a.mapError(endedSeen.Load(), fmt.Errorf("%w: empty transcript", errEmptyTranscript))
		}
		emptyUtterances = 0
		userTurns++

		turn, err := session.SendUserTurn(runCtx, text)
		if err != nil {
			return domain.Outcome{}, a.mapError(endedSeen.Load(), fmt.Errorf("dialogue user turn: %w", err))
		}
		outcome, err := a.processTurn(runCtx, call, session, tools, turn)
		if err != nil {
			return domain.Outcome{}, a.mapError(endedSeen.Load(), err)
		}
		if outcome != nil {
			a.log.Info("agent: dialogue outcome",
				"order_id", order.ID,
				"outcome_kind", outcome.Kind,
				"fail_reason", outcome.FailReason,
				"user_turns", userTurns,
			)
			return *outcome, nil
		}
	}
}

// processTurn speaks one assistant turn and follows tool continuations until the model
// produces an outcome or a text-only reply. A nil outcome with nil error means the agent
// should return to listening.
func (a *Agent) processTurn(ctx context.Context, call AudioSession, session ports.DialogueSession,
	tools ToolExecutor, turn ports.AssistantTurn) (*domain.Outcome, error) {
	toolRounds := 0
	for {
		spoke, err := a.speak(ctx, call, turn.TextChunks())
		if err != nil {
			return nil, err
		}
		result, err := turn.Wait(ctx)
		if err != nil {
			return nil, fmt.Errorf("dialogue turn wait: %w", err)
		}

		if result.Outcome != nil {
			if len(result.ToolCalls) > 0 {
				return nil, errors.New("agent: outcome turn unexpectedly contains tool calls")
			}
			if err := tools.ValidateOutcome(result.Outcome); err != nil {
				return nil, fmt.Errorf("outcome validation: %w", err)
			}
			return result.Outcome, nil
		}

		if len(result.ToolCalls) > 0 {
			if toolRounds >= a.cfg.MaxToolRounds {
				return nil, errToolRoundsExceeded
			}
			toolRounds++
			results, err := a.executeTools(ctx, tools, result.ToolCalls)
			if err != nil {
				return nil, err
			}
			next, err := session.SubmitToolResults(ctx, results)
			if err != nil {
				return nil, fmt.Errorf("dialogue tool results: %w", err)
			}
			turn = next
			continue
		}

		if !spoke {
			return nil, errEmptyTurn
		}
		return nil, nil
	}
}

// executeTools runs ordinary calls sequentially in model order, producing exactly one
// JSON result per call ID. finish_call is reserved and must never reach this function.
func (a *Agent) executeTools(ctx context.Context, tools ToolExecutor, calls []ports.ToolCall) ([]ports.ToolResult, error) {
	results := make([]ports.ToolResult, 0, len(calls))
	for _, call := range calls {
		if call.Name == ToolFinishCall {
			return nil, errors.New("agent: finish_call must be intercepted by the dialogue adapter")
		}
		result, err := tools.Execute(ctx, call)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", call.Name, err)
		}
		if !jsonValidObject(result) {
			return nil, fmt.Errorf("agent: tool %s returned invalid JSON", call.Name)
		}
		results = append(results, ports.ToolResult{ID: call.ID, Result: result})
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
// operation, so the runner uses the actual terminal event for its fallback outcome.
func (a *Agent) mapError(ended bool, err error) error {
	if ended {
		return ErrCallEnded
	}
	switch {
	case errors.Is(err, errListenTimeout),
		errors.Is(err, errEmptyTranscript),
		errors.Is(err, errUserTurnsExceeded),
		errors.Is(err, errToolRoundsExceeded):
		return markLimit(err)
	default:
		return err
	}
}
