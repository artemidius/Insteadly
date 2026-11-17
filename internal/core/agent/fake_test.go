package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// --- order store fake -----------------------------------------------------------

type fakeStore struct {
	mu       sync.Mutex
	orders   map[domain.OrderID]domain.Order
	history  []domain.Order
	updateFn func(*domain.Order) error
	updates  int
}

func newFakeStore(orders ...domain.Order) *fakeStore {
	s := &fakeStore{orders: make(map[domain.OrderID]domain.Order, len(orders))}
	for _, o := range orders {
		if o.ID == "" {
			o.ID = domain.NewOrderID()
		}
		s.orders[o.ID] = o
	}
	return s
}

func (s *fakeStore) Create(_ context.Context, o domain.Order) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.ID == "" {
		o.ID = domain.NewOrderID()
	}
	if _, exists := s.orders[o.ID]; exists {
		return domain.Order{}, ports.ErrConflict
	}
	o.Status = domain.OrderCreated
	s.orders[o.ID] = o
	return o, nil
}

func (s *fakeStore) Get(_ context.Context, id domain.OrderID) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	return o, nil
}

func (s *fakeStore) List(context.Context) ([]domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out, nil
}

func (s *fakeStore) Update(_ context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return domain.Order{}, ports.ErrNotFound
	}
	if s.updateFn != nil {
		if err := s.updateFn(&o); err != nil {
			return domain.Order{}, err
		}
	}
	if err := fn(&o); err != nil {
		return domain.Order{}, err
	}
	s.orders[id] = o
	s.history = append(s.history, o)
	s.updates++
	return o, nil
}

func (s *fakeStore) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates
}

// --- STT fake -------------------------------------------------------------------

type fakeSTTScript struct {
	partials      []string
	finals        []string
	openErr       error
	feedErr       error
	closeErr      error
	resultsErr    bool   // close results without any final
	partialOnFeed string // emitted once on the first Feed, before Close
}

type fakeSTT struct {
	mu       sync.Mutex
	script   fakeSTTScript
	scripts  []fakeSTTScript
	opens    int
	closes   int
	feeds    [][]byte
	opts     []ports.StreamOptions
	streams  []*fakeTranscriptionStream
	closeCtx []error
}

func (f *fakeSTT) TranscribeStream(ctx context.Context, opts ports.StreamOptions) (ports.TranscriptionStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.script.openErr != nil {
		return nil, f.script.openErr
	}
	f.opens++
	f.opts = append(f.opts, opts)
	script := f.script
	if len(f.scripts) > 0 {
		script = f.scripts[0]
		f.scripts = f.scripts[1:]
	}
	s := &fakeTranscriptionStream{owner: f, script: script, results: make(chan ports.TranscriptionEvent, 16)}
	f.streams = append(f.streams, s)
	return s, nil
}

func (f *fakeSTT) TranscribeFile(context.Context, io.Reader, ports.BatchOptions) (domain.Transcript, error) {
	return domain.Transcript{}, errors.New("TranscribeFile must not be called in C3")
}

type fakeTranscriptionStream struct {
	owner   *fakeSTT
	script  fakeSTTScript
	results chan ports.TranscriptionEvent
	once    sync.Once
}

func (s *fakeTranscriptionStream) Feed(ctx context.Context, chunk domain.AudioChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.script.feedErr != nil {
		return s.script.feedErr
	}
	s.owner.feeds = append(s.owner.feeds, append([]byte(nil), chunk.Data...))
	if s.script.partialOnFeed != "" {
		s.results <- ports.TranscriptionEvent{Text: s.script.partialOnFeed}
		s.script.partialOnFeed = ""
	}
	return nil
}

func (s *fakeTranscriptionStream) Results() <-chan ports.TranscriptionEvent { return s.results }

func (s *fakeTranscriptionStream) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.owner.mu.Lock()
		s.owner.closes++
		s.owner.closeCtx = append(s.owner.closeCtx, ctx.Err())
		s.owner.mu.Unlock()
		if !s.script.resultsErr {
			for _, p := range s.script.partials {
				s.results <- ports.TranscriptionEvent{Text: p}
			}
			for _, final := range s.script.finals {
				s.results <- ports.TranscriptionEvent{Text: final, Final: true}
			}
		}
		close(s.results)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.script.closeErr
}

// --- TTS fake -------------------------------------------------------------------

type fakeTTSScript struct {
	openErr   error
	feedErr   error
	closeErr  error
	offline   bool
	chunkSize int
}

type fakeTTS struct {
	mu       sync.Mutex
	script   fakeTTSScript
	opens    int
	closes   int
	aborts   int
	texts    []string
	streams  []*fakeSpeechStream
	feedHook func(text string)
}

func (f *fakeTTS) Speak(ctx context.Context, _ ports.SpeakOptions) (ports.SpeechStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.script.openErr != nil {
		return nil, f.script.openErr
	}
	f.opens++
	s := &fakeSpeechStream{owner: f, audio: make(chan domain.AudioChunk, 64)}
	f.streams = append(f.streams, s)
	return s, nil
}

type fakeSpeechStream struct {
	owner  *fakeTTS
	audio  chan domain.AudioChunk
	once   sync.Once
	buffer []string

	sendMu sync.Mutex
	closed bool
}

func (s *fakeSpeechStream) FeedText(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.owner.mu.Lock()
	script := s.owner.script
	s.owner.texts = append(s.owner.texts, text)
	hook := s.owner.feedHook
	s.owner.mu.Unlock()
	if hook != nil {
		hook(text)
	}
	if script.feedErr != nil {
		return script.feedErr
	}
	if script.offline {
		s.buffer = append(s.buffer, text)
		return nil
	}
	s.emit()
	return nil
}

func (s *fakeSpeechStream) Audio() <-chan domain.AudioChunk { return s.audio }

// Abort discards without flushing.
func (s *fakeSpeechStream) Abort(ctx context.Context) error {
	s.once.Do(func() {
		s.owner.mu.Lock()
		s.owner.aborts++
		s.owner.mu.Unlock()
		s.closeAudio()
	})
	return nil
}

func (s *fakeSpeechStream) closeAudio() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.audio)
	}
}

func (s *fakeSpeechStream) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.owner.mu.Lock()
		s.owner.closes++
		script := s.owner.script
		s.owner.mu.Unlock()
		if script.offline {
			for range s.buffer {
				s.emit()
			}
		}
		s.closeAudio()
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	return s.owner.script.closeErr
}

func (s *fakeSpeechStream) emit() {
	size := s.owner.script.chunkSize
	if size <= 0 {
		size = domain.FrameBytes
	}
	chunk := make([]byte, size)
	for i := range chunk {
		chunk[i] = 1
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.audio <- domain.AudioChunk{Data: chunk}:
	default:
		// Tests with a slow drainer use a larger queue via chunkSize.
	}
}

// --- audio session fake ---------------------------------------------------------

type fakeCall struct {
	inbound  chan domain.AudioChunk
	controls chan domain.CallEvent

	mu         sync.Mutex
	sent       [][]byte
	sendErr    error
	clearErr   error
	markErr    error
	clears     int
	marks      []string
	dtmf       []string
	dtmfErr    error
	markSilent bool
	markHook   func(id string)
}

func newFakeCall(inboundChunks ...[]byte) *fakeCall {
	c := &fakeCall{
		inbound:  make(chan domain.AudioChunk, 256),
		controls: make(chan domain.CallEvent, 256),
	}
	for _, chunk := range inboundChunks {
		c.inbound <- domain.AudioChunk{Data: chunk}
	}
	return c
}

func (c *fakeCall) Control() <-chan domain.CallEvent { return c.controls }

// push enqueues canonical audio after the call started, for tests that must not interrupt
// the greeting.
func (c *fakeCall) push(chunks ...[]byte) {
	for _, chunk := range chunks {
		c.inbound <- domain.AudioChunk{Data: chunk}
	}
}

func (c *fakeCall) Inbound() <-chan domain.AudioChunk { return c.inbound }

func (c *fakeCall) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, append([]byte(nil), chunk.Data...))
	return nil
}

func (c *fakeCall) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *fakeCall) SendDTMF(ctx context.Context, digits string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.dtmf = append(c.dtmf, digits)
	err := c.dtmfErr
	c.mu.Unlock()
	return err
}

func (c *fakeCall) ClearAudio(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clears++
	c.sent = nil
	return c.clearErr
}

// MarkPlayback acknowledges immediately through the normalized control channel: the fake
// has no provider queue, so the mark is reached after all previously accepted audio.
func (c *fakeCall) MarkPlayback(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.markErr != nil {
		err := c.markErr
		c.mu.Unlock()
		return err
	}
	c.marks = append(c.marks, id)
	hook := c.markHook
	silent := c.markSilent
	c.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if silent {
		return nil
	}
	select {
	case c.controls <- domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: id}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeCall) clearCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clears
}

func (c *fakeCall) markIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.marks...)
}

func (c *fakeCall) closeInbound() { close(c.inbound) }

// --- dialogue fake --------------------------------------------------------------

type turnScript struct {
	chunks  []string
	result  ports.TurnResult
	err     error
	waitCtx <-chan struct{}
	waitErr error
}

type fakeDialogue struct {
	mu         sync.Mutex
	scripts    []turnScript
	requests   []ports.SessionRequest
	opens      int
	closed     int
	texts      []string
	results    [][]ports.ToolResult
	interrupts []string
	controls   []string
	failOpen   error
}

func (d *fakeDialogue) NewSession(ctx context.Context, req ports.SessionRequest) (ports.DialogueSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failOpen != nil {
		return nil, d.failOpen
	}
	d.opens++
	d.requests = append(d.requests, req)
	return &fakeDialogueSession{owner: d}, nil
}

type fakeDialogueSession struct {
	owner *fakeDialogue
}

func (s *fakeDialogueSession) SendUserTurn(ctx context.Context, text string) (ports.AssistantTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.owner.mu.Lock()
	s.owner.texts = append(s.owner.texts, text)
	s.owner.mu.Unlock()
	return s.next("user")
}

func (s *fakeDialogueSession) SubmitToolResults(ctx context.Context, results []ports.ToolResult) (ports.AssistantTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.owner.mu.Lock()
	s.owner.results = append(s.owner.results, results)
	s.owner.mu.Unlock()
	return s.next("tool")
}

func (s *fakeDialogueSession) next(string) (ports.AssistantTurn, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if len(s.owner.scripts) == 0 {
		return newFakeTurn(nil, ports.TurnResult{}), nil
	}
	script := s.owner.scripts[0]
	s.owner.scripts = s.owner.scripts[1:]
	if script.err != nil {
		return nil, script.err
	}
	turn := newFakeTurn(script.chunks, script.result)
	turn.waitCtx = script.waitCtx
	turn.waitErr = script.waitErr
	return turn, nil
}

func (s *fakeDialogueSession) Interrupt(ctx context.Context, confirmedText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.owner.mu.Lock()
	s.owner.interrupts = append(s.owner.interrupts, confirmedText)
	s.owner.mu.Unlock()
	return nil
}

func (s *fakeDialogueSession) SendControl(ctx context.Context, instruction string) (ports.AssistantTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.owner.mu.Lock()
	s.owner.controls = append(s.owner.controls, instruction)
	s.owner.mu.Unlock()
	return s.next("control")
}

func (s *fakeDialogueSession) Close(context.Context) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.closed++
	return nil
}

type fakeTurn struct {
	chunks  chan string
	result  ports.TurnResult
	waitCtx <-chan struct{}
	waitErr error
}

func newFakeTurn(chunks []string, result ports.TurnResult) *fakeTurn {
	ch := make(chan string, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return &fakeTurn{chunks: ch, result: result}
}

func (t *fakeTurn) TextChunks() <-chan string { return t.chunks }

func (t *fakeTurn) Wait(ctx context.Context) (ports.TurnResult, error) {
	if t.waitErr != nil && t.waitCtx == nil {
		return t.result, t.waitErr
	}
	if t.waitCtx != nil {
		select {
		case <-t.waitCtx:
			return ports.TurnResult{}, t.waitErr
		case <-ctx.Done():
			return ports.TurnResult{}, ctx.Err()
		}
	}
	return t.result, nil
}

// --- tool executor fake ---------------------------------------------------------

type fakeToolExecutor struct {
	execErr     error
	validateErr error
	executed    []ports.ToolCall
}

func (f *fakeToolExecutor) Execute(_ context.Context, call ports.ToolCall) ([]byte, error) {
	f.executed = append(f.executed, call)
	if f.execErr != nil {
		return nil, f.execErr
	}
	return []byte(`{"ok":true,"field":"note"}`), nil
}

func (f *fakeToolExecutor) ValidateOutcome(*domain.Outcome) error { return f.validateErr }

func testOrder() domain.Order {
	return domain.Order{
		ID:     domain.NewOrderID(),
		Status: domain.OrderScheduled,
		Clinic: domain.Clinic{Name: "Ordination Dr. Test", Phone: "+431234567"},
		Patient: domain.Patient{
			FullName:        "Max Mustermann",
			Phone:           "+436641234567",
			BirthDate:       "1980-01-01",
			InsuranceNumber: "AT-123",
			Reason:          "Kontrolle",
			PreferredSlots: []domain.TimeRange{
				{
					Start: time.Date(2027, 1, 15, 8, 0, 0, 0, time.FixedZone("CET", 3600)),
					End:   time.Date(2027, 1, 15, 12, 0, 0, 0, time.FixedZone("CET", 3600)),
				},
			},
		},
	}
}

var fixedNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func testConfig(store ports.OrderStore) Config {
	return Config{
		STT:      &fakeSTT{},
		TTS:      &fakeTTS{},
		Dialogue: &fakeDialogue{},
		Orders:   store,
		Now:      func() time.Time { return fixedNow },
		VAD:      testVADConfig(),
		Hold:     testHoldConfig(),
	}
}

func fastAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// noOutcomeDialogue returns a dialogue that always answers with a short text-only turn.
func textOnlyDialogue(text string) *fakeDialogue {
	return &fakeDialogue{scripts: []turnScript{{chunks: []string{text}}}}
}
