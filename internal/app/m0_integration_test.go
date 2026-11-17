package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	dialoguemistral "github.com/artemidius/insteadly/internal/adapters/dialogue/mistral"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	sttmistral "github.com/artemidius/insteadly/internal/adapters/stt/mistral"
	ttsmistral "github.com/artemidius/insteadly/internal/adapters/tts/mistral"
	"github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/runner"
)

const integrationAPIKey = "fake-mistral-key-must-not-leak"

const (
	integrationClinic = "Ordination Dr. Integration"
	slotStart         = "2027-01-15T09:00:00+01:00"
	slotEnd           = "2027-01-15T09:30:00+01:00"
)

// --- fake Mistral server --------------------------------------------------------

type wireCall struct {
	id   string
	name string
	args string
}

type chatScript struct {
	texts []string
	calls []wireCall
}

type chatRequest struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type fakeMistral struct {
	t *testing.T

	mu            sync.Mutex
	sttAudio      [][]byte
	sttEnds       int
	sttHints      []string
	sttLanguage   string
	ttsTexts      []string
	ttsConns      int
	chatCount     int
	toolResults   []string
	toolNames     []string
	chatResponses func(n int, req chatRequest) chatScript

	sttFinals   []string
	sttStreams  int
	ttsDelay    map[string]time.Duration
	chatHistory []chatRequest
}

func newFakeMistral(t *testing.T, responses func(n int, req chatRequest) chatScript) (*fakeMistral, *httptest.Server) {
	t.Helper()
	f := &fakeMistral{t: t, chatResponses: responses, ttsDelay: map[string]time.Duration{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/transcriptions/realtime", f.serveSTT)
	mux.HandleFunc("/v1/audio/speech/realtime", f.serveTTS)
	mux.HandleFunc("/v1/chat/completions", f.serveChat)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMistral) serveSTT(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("stt accept: %v", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	f.mu.Lock()
	f.sttStreams++
	streamIndex := f.sttStreams - 1
	f.mu.Unlock()
	wsWriteJSON(f.t, conn, map[string]any{"type": "session.created", "session": map[string]any{"model": "fake"}})
	update := wsReadJSON(f.t, conn)
	if session, ok := update["session"].(map[string]any); ok {
		if language, ok := session["language"].(string); ok {
			f.mu.Lock()
			f.sttLanguage = language
			f.mu.Unlock()
		}
		if hints, ok := session["context_bias"].([]any); ok {
			var values []string
			for _, h := range hints {
				if s, ok := h.(string); ok {
					values = append(values, s)
				}
			}
			f.mu.Lock()
			f.sttHints = values
			f.mu.Unlock()
		}
	}
	wsWriteJSON(f.t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})

	for {
		message := wsReadJSON(f.t, conn)
		switch message["type"] {
		case "input_audio.append":
			raw, err := base64.StdEncoding.DecodeString(message["audio"].(string))
			if err != nil {
				f.t.Errorf("stt audio decode: %v", err)
				return
			}
			f.mu.Lock()
			f.sttAudio = append(f.sttAudio, raw)
			f.mu.Unlock()
		case "input_audio.end":
			f.mu.Lock()
			f.sttEnds++
			final := "Ja, wir haben am 15. Jänner um neun Uhr einen Termin, der passt, ich bestätige das."
			if streamIndex < len(f.sttFinals) {
				final = f.sttFinals[streamIndex]
			}
			f.mu.Unlock()
			partial := final
			if len(partial) > 12 {
				partial = partial[:12]
			}
			wsWriteJSON(f.t, conn, map[string]any{"type": "transcription.text.delta", "text": partial})
			wsWriteJSON(f.t, conn, map[string]any{
				"type":     "transcription.done",
				"text":     final,
				"segments": []any{}, "language": "de",
			})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func (f *fakeMistral) serveTTS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("tts accept: %v", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	f.mu.Lock()
	f.ttsConns++
	f.mu.Unlock()
	wsWriteJSON(f.t, conn, map[string]any{"type": "session.created", "session": map[string]any{}})
	_ = wsReadJSON(f.t, conn) // session.update
	wsWriteJSON(f.t, conn, map[string]any{"type": "session.updated", "session": map[string]any{}})
	for {
		message := wsReadJSON(f.t, conn)
		switch message["type"] {
		case "input_text.append":
			f.mu.Lock()
			f.ttsTexts = append(f.ttsTexts, message["text"].(string))
			f.mu.Unlock()
		case "input_text.end":
			f.mu.Lock()
			texts := append([]string(nil), f.ttsTexts...)
			f.mu.Unlock()
			delay := time.Duration(0)
			if len(texts) > 0 {
				delay = f.ttsDelay[texts[len(texts)-1]]
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			audio := sineFloat32(480, 440)
			half := len(audio) / 2
			// The core may abort this generation at any point; a write failure is expected
			// and must not be reported as a test error.
			_ = writeWSBestEffort(conn, map[string]any{"type": "speech.audio.delta", "audio_data": base64.StdEncoding.EncodeToString(float32LE(audio[:half]))})
			_ = writeWSBestEffort(conn, map[string]any{"type": "speech.audio.delta", "audio_data": base64.StdEncoding.EncodeToString(float32LE(audio[half:]))})
			_ = writeWSBestEffort(conn, map[string]any{"type": "speech.audio.done"})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func (f *fakeMistral) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("chat decode: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.chatCount++
	n := f.chatCount
	f.chatHistory = append(f.chatHistory, req)
	for _, message := range req.Messages {
		if message.Role == "tool" {
			f.toolResults = append(f.toolResults, message.Content)
		}
	}
	f.toolNames = f.toolNames[:0]
	for _, tool := range req.Tools {
		f.toolNames = append(f.toolNames, tool.Function.Name)
	}
	f.mu.Unlock()

	script := f.chatResponses(n, req)
	w.Header().Set("Content-Type", "text/event-stream")
	var b strings.Builder
	for _, text := range script.texts {
		b.WriteString(sseLine(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
		}))
	}
	for i, call := range script.calls {
		b.WriteString(sseLine(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": i, "id": call.id, "type": "function",
					"function": map[string]any{"name": call.name, "arguments": call.args},
				}},
			}, "finish_reason": nil}},
		}))
	}
	finish := "tool_calls"
	if len(script.calls) == 0 {
		finish = "stop"
	}
	b.WriteString(sseLine(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}))
	b.WriteString("data: [DONE]\n\n")
	_, _ = io.WriteString(w, b.String())
}

func sseLine(v any) string {
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

func writeWSBestEffort(conn *websocket.Conn, v any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, v)
}

func wsWriteJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, v); err != nil {
		t.Errorf("ws write: %v", err)
	}
}

func wsReadJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v map[string]any
	if err := wsjson.Read(ctx, conn, &v); err != nil {
		t.Errorf("ws read: %v", err)
	}
	return v
}

func sineFloat32(n int, freq float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / 24000))
	}
	return out
}

func float32LE(samples []float32) []byte {
	out := make([]byte, 4*len(samples))
	for i, s := range samples {
		bits := math.Float32bits(s)
		binary.LittleEndian.PutUint32(out[4*i:], bits)
	}
	return out
}

// --- fake telephony -------------------------------------------------------------

type integrationTelephony struct {
	session *integrationSession
}

func (t *integrationTelephony) Dial(context.Context, domain.Order) (ports.CallSession, error) {
	return readyIntegrationSession{session: t.session}, nil
}

type integrationSession struct {
	events  chan domain.CallEvent
	inbound chan domain.AudioChunk

	sendMu        sync.Mutex
	mu            sync.Mutex
	hangupCount   int
	audioAtHangup int
	sentAudio     [][]byte
	marksAcked    int
	clearCount    int
	dtmf          []string
	ended         bool
}

type readyIntegrationSession struct{ session *integrationSession }

func (r readyIntegrationSession) StreamReady() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (r readyIntegrationSession) ID() domain.CallID               { return "call-integration" }
func (r readyIntegrationSession) Events() <-chan domain.CallEvent { return r.session.events }
func (r readyIntegrationSession) Inbound() <-chan domain.AudioChunk {
	return r.session.inbound
}

func (r readyIntegrationSession) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	r.session.sentAudio = append(r.session.sentAudio, append([]byte(nil), chunk.Data...))
	return nil
}

func (r readyIntegrationSession) SendDTMF(_ context.Context, digits string) error {
	r.session.mu.Lock()
	r.session.dtmf = append(r.session.dtmf, digits)
	r.session.mu.Unlock()
	return nil
}

func (r readyIntegrationSession) ClearAudio(context.Context) error {
	r.session.mu.Lock()
	r.session.clearCount++
	r.session.sentAudio = nil
	r.session.mu.Unlock()
	return nil
}

func (r readyIntegrationSession) MarkPlayback(_ context.Context, id string) error {
	r.session.mu.Lock()
	if r.session.ended {
		r.session.mu.Unlock()
		return nil
	}
	r.session.marksAcked++
	r.session.mu.Unlock()
	r.session.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventPlaybackMark, PlaybackID: id, At: time.Now()}
	return nil
}

func (r readyIntegrationSession) Hangup(context.Context) error {
	r.session.mu.Lock()
	r.session.hangupCount++
	r.session.audioAtHangup = len(r.session.sentAudio)
	ended := r.session.ended
	r.session.mu.Unlock()
	if !ended {
		r.session.end(domain.EndAborted)
	}
	return nil
}

func (s *integrationSession) end(reason domain.EndReason) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventEnded, EndReason: reason, At: time.Now()}
	close(s.events)
	close(s.inbound)
}

func newIntegrationSession() *integrationSession {
	return &integrationSession{
		events:  make(chan domain.CallEvent, 16),
		inbound: make(chan domain.AudioChunk, 4096),
	}
}

func (s *integrationSession) sendProgress() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventDialing, At: time.Now()}
	s.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventRinging, At: time.Now()}
	s.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventAnswered, At: time.Now()}
}

// utteranceFrames is the canonical deterministic speech fixture: 20 non-stationary voiced
// frames followed by the 700 ms hangover.
func utteranceFrames() [][]byte {
	out := make([][]byte, 0, 55)
	for i := 0; i < 20; i++ {
		out = append(out, speechPCMFrame(i))
	}
	for i := 0; i < 35; i++ {
		out = append(out, make([]byte, domain.FrameBytes))
	}
	return out
}

// speechPCMFrame generates a non-stationary multi-component frame that passes the core VAD
// without any human recording.
func speechPCMFrame(seed int) []byte {
	freqs := [6]float64{220, 730, 1150, 1750, 2650, 3300}
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		var v float64
		for k := 0; k < len(freqs); k++ {
			amp := 0.10 + 0.08*math.Sin(float64(seed*7+k*3))
			phase := float64(seed*11+k*5) * 0.37
			v += amp * math.Sin(2*math.Pi*freqs[k]*float64(i)/16000+phase)
		}
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(int16(v*18000)))
	}
	return frame
}

// trySendFrame enqueues one canonical frame unless the call already ended.
func (s *integrationSession) trySendFrame(frame []byte) bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended {
		return false
	}
	s.inbound <- domain.AudioChunk{Data: frame}
	return true
}

// sendFrames bursts canonical chunks for deterministic credential-free fixtures. Live
// telephony must NOT use this: real inbound audio arrives at 20 ms/frame (50 fps), and a
// multi-second burst can overflow the bounded STT worker queue while a real WebSocket
// handshake is in flight. m1_live_test.go paces the live no-phone path at canonical real
// time; a burst is a test-harness artifact, not a VAD/queue defect.
func (s *integrationSession) sendFrames(frames [][]byte) {
	for _, frame := range frames {
		if !s.trySendFrame(frame) {
			return
		}
	}
}

// --- wiring ---------------------------------------------------------------------

type integrationHarness struct {
	store     *local.Store
	order     domain.Order
	telephony *integrationTelephony
	session   *integrationSession
	mistral   *fakeMistral
	agent     *agent.Agent
	runner    *runner.Runner
	logs      *bytes.Buffer
	baseURL   string
}

func newHarness(t *testing.T, responses func(n int, req chatRequest) chatScript) *integrationHarness {
	t.Helper()
	return newHarnessWith(t, responses, nil)
}

func newHarnessWith(t *testing.T, responses func(n int, req chatRequest) chatScript, mutate func(*agent.Config)) *integrationHarness {
	t.Helper()
	mistral, srv := newFakeMistral(t, responses)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stt, err := sttmistral.New(sttmistral.Config{APIKey: integrationAPIKey, BaseURL: srv.URL, HTTPClient: &http.Client{}, Logger: logger})
	if err != nil {
		t.Fatalf("stt New: %v", err)
	}
	tts, err := ttsmistral.New(ttsmistral.Config{APIKey: integrationAPIKey, BaseURL: srv.URL, HTTPClient: &http.Client{}, Logger: logger})
	if err != nil {
		t.Fatalf("tts New: %v", err)
	}
	dialogue, err := dialoguemistral.New(dialoguemistral.Config{APIKey: integrationAPIKey, BaseURL: srv.URL, HTTPClient: &http.Client{}, Logger: logger})
	if err != nil {
		t.Fatalf("dialogue New: %v", err)
	}
	storage, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage New: %v", err)
	}

	order := domain.Order{
		Status: domain.OrderScheduled,
		Clinic: domain.Clinic{Name: integrationClinic, Phone: "+431234567"},
		Patient: domain.Patient{
			FullName: "Synthetic Patient",
			Reason:   "Kontrolle",
			PreferredSlots: []domain.TimeRange{{
				Start: time.Date(2027, 1, 15, 8, 0, 0, 0, time.FixedZone("CET", 3600)),
				End:   time.Date(2027, 1, 15, 12, 0, 0, 0, time.FixedZone("CET", 3600)),
			}},
		},
	}
	created, err := storage.Orders().Create(context.Background(), order)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := storage.Orders().Update(context.Background(), created.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Transition(domain.OrderScheduled)
	}); err != nil {
		t.Fatalf("seed scheduled: %v", err)
	}

	agentCfg := agent.Config{
		STT: stt, TTS: tts, Dialogue: dialogue, Orders: storage.Orders(), Logger: logger,
	}
	if mutate != nil {
		mutate(&agentCfg)
	}
	ag, err := agent.New(agentCfg)
	if err != nil {
		t.Fatalf("agent New: %v", err)
	}
	session := newIntegrationSession()
	telephony := &integrationTelephony{session: session}
	run, err := runner.New(runner.Config{
		Telephony: telephony,
		Orders:    storage.Orders(),
		Agent:     ag,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("runner New: %v", err)
	}
	return &integrationHarness{
		store: storage, order: created, telephony: telephony,
		session: session, mistral: mistral, agent: ag, runner: run, logs: logs, baseURL: srv.URL,
	}
}

type harnessRun struct {
	final domain.Order
	err   error
}

// start launches one runner attempt and returns its result channel.
func (h *integrationHarness) start(t *testing.T) <-chan harnessRun {
	t.Helper()
	done := make(chan harnessRun, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		final, err := h.runner.Run(ctx, h.order.ID)
		done <- harnessRun{final: final, err: err}
	}()
	return done
}

func (h *integrationHarness) waitRun(t *testing.T, done <-chan harnessRun) harnessRun {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not return")
		return harnessRun{}
	}
}

func (h *integrationHarness) awaitGreeting(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.session.mu.Lock()
		n, ended := h.session.marksAcked, h.session.ended
		h.session.mu.Unlock()
		if n >= 2 || ended {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("greeting never confirmed")
}

func (h *integrationHarness) run(t *testing.T) (domain.Order, error) {
	t.Helper()
	h.session.sendProgress()
	go func() {
		// Wait until both fixed greeting segments were confirmed before speaking, so the
		// scripted utterance is an ordinary turn instead of a greeting barge.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			h.session.mu.Lock()
			n := h.session.marksAcked
			ended := h.session.ended
			h.session.mu.Unlock()
			if n >= 2 || ended {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		h.session.sendFrames(utteranceFrames())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.runner.Run(ctx, h.order.ID)
}

// bookedScript is the deterministic three-turn booked conversation.
func bookedScript() func(n int, req chatRequest) chatScript {
	propose := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A"}`
	confirm := `{"start":"` + slotStart + `","end":"` + slotEnd + `","doctor":"Dr. A","practice":"Praxis A","confirmation_code":"TEST-123"}`
	booked := `{"kind":"booked","detail":"Termin bestätigt","booked_slot":{"date_time":"` + slotStart + `"}}`
	return func(n int, _ chatRequest) chatScript {
		switch n {
		case 1:
			return chatScript{texts: []string{"Einen Moment bitte. "}, calls: []wireCall{{id: "call_propose", name: "propose_slot", args: propose}}}
		case 2:
			return chatScript{calls: []wireCall{{id: "call_confirm", name: "confirm_booking", args: confirm}}}
		case 3:
			return chatScript{texts: []string{"Gerne, der Termin ist gebucht."}, calls: []wireCall{{id: "call_finish", name: "finish_call", args: booked}}}
		default:
			return chatScript{texts: []string{"unexpected"}}
		}
	}
}

func TestM0IntegrationBookedFullChain(t *testing.T) {
	h := newHarness(t, bookedScript())
	final, err := h.run(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Status != domain.OrderFinished {
		t.Fatalf("status = %q, want finished", final.Status)
	}
	if final.Outcome == nil || final.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v", final.Outcome)
	}
	wantSlot := time.Date(2027, 1, 15, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	if !final.Outcome.BookedSlot.DateTime.Equal(wantSlot) {
		t.Errorf("booked slot = %v, want %v", final.Outcome.BookedSlot.DateTime, wantSlot)
	}
	if final.Outcome.BookedSlot.Doctor != "Dr. A" || final.Outcome.BookedSlot.Practice != "Praxis A" {
		t.Errorf("booked doctor/practice = %+v", final.Outcome.BookedSlot)
	}
	if final.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", final.Attempts)
	}
	if final.Patient.Reason != "Kontrolle" || final.Patient.FullName != "Synthetic Patient" {
		t.Errorf("patient fields changed: %+v", final.Patient)
	}

	// STT: canonical audio + end, one hint (clinic name).
	h.mistral.mu.Lock()
	sttAudio := append([][]byte(nil), h.mistral.sttAudio...)
	sttEnds := h.mistral.sttEnds
	hints := append([]string(nil), h.mistral.sttHints...)
	language := h.mistral.sttLanguage
	ttsTexts := append([]string(nil), h.mistral.ttsTexts...)
	toolResults := append([]string(nil), h.mistral.toolResults...)
	toolNames := append([]string(nil), h.mistral.toolNames...)
	chatCount := h.mistral.chatCount
	h.mistral.mu.Unlock()

	if sttEnds != 1 {
		t.Errorf("STT input_audio.end count = %d, want 1", sttEnds)
	}
	// Streaming VAD opens STT at confirmed onset with the bounded pre-roll, then forwards
	// live frames through the hangover. The exact onset frame depends on floating-point DSP
	// tolerances across platforms, so assert the stronger invariant instead: the fed audio
	// is one contiguous inbound window with no replay, reordering, or duplication, and it
	// includes the hangover.
	inbound := utteranceFrames()
	if len(sttAudio) < 35 || len(sttAudio) > len(inbound) {
		t.Fatalf("STT chunks = %d, want a contiguous window of at least the hangover", len(sttAudio))
	}
	for _, chunk := range sttAudio {
		if len(chunk) != domain.FrameBytes {
			t.Fatalf("STT audio chunk length = %d, want %d", len(chunk), domain.FrameBytes)
		}
	}
	start := -1
	for i := 0; i+len(sttAudio) <= len(inbound); i++ {
		match := true
		for j := range sttAudio {
			if !bytes.Equal(sttAudio[j], inbound[i+j]) {
				match = false
				break
			}
		}
		if match {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("STT audio is not one contiguous, ordered inbound window")
	}
	if language != "de" {
		t.Errorf("STT language = %q, want de", language)
	}
	if len(hints) != 1 || hints[0] != integrationClinic {
		t.Errorf("STT hints = %v", hints)
	}

	// Dialogue: the exact propose/confirm results, no finish_call tool result.
	if chatCount != 3 {
		t.Errorf("chat requests = %d, want 3", chatCount)
	}
	wantPropose := `{"ok":true,"start":"2027-01-15T09:00:00+01:00","end":"2027-01-15T09:30:00+01:00"}`
	wantConfirm := `{"ok":true,"confirmation_code":"TEST-123"}`
	// Request 2 carries the propose result; request 3 carries the same propose history
	// plus the confirm result. finish_call must never produce a tool result.
	if len(toolResults) != 3 {
		t.Fatalf("tool result history = %d, want 3 (propose, propose, confirm)", len(toolResults))
	}
	if toolResults[0] != wantPropose || toolResults[1] != wantPropose {
		t.Errorf("propose result history = %q", toolResults[:2])
	}
	if toolResults[2] != wantConfirm {
		t.Errorf("confirm result = %s, want %s", toolResults[2], wantConfirm)
	}
	wantTools := []string{"propose_slot", "confirm_booking", "capture_info", "escalate_to_human", "finish_call"}
	if len(toolNames) != len(wantTools) {
		t.Errorf("tools offered = %v", toolNames)
	} else {
		for i := range wantTools {
			if toolNames[i] != wantTools[i] {
				t.Errorf("tool[%d] = %q, want %q", i, toolNames[i], wantTools[i])
			}
		}
	}

	// TTS: separate disclosure + purpose greeting segments, then one bounded segment per
	// assistant text segment.
	if len(ttsTexts) != 4 {
		t.Fatalf("TTS texts = %d, want 4 (disclosure, purpose, reply 1, reply 2): %q", len(ttsTexts), ttsTexts)
	}
	if !strings.Contains(ttsTexts[0], "KI-Assistent") {
		t.Errorf("AI disclosure was not spoken: %q", ttsTexts[0])
	}
	if !strings.Contains(ttsTexts[1], "Synthetic Patient") {
		t.Errorf("greeting purpose was not spoken: %q", ttsTexts[1])
	}
	if ttsTexts[2] != "Einen Moment bitte." {
		t.Errorf("turn-1 text = %q", ttsTexts[2])
	}
	if ttsTexts[3] != "Gerne, der Termin ist gebucht." {
		t.Errorf("final text = %q", ttsTexts[3])
	}

	// Outbound canonical audio: every frame 640 bytes, at least one non-zero sample.
	h.session.mu.Lock()
	sent := append([][]byte(nil), h.session.sentAudio...)
	hangups := h.session.hangupCount
	audioAtHangup := h.session.audioAtHangup
	h.session.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("no outbound canonical audio")
	}
	nonZeroOut := false
	for _, frame := range sent {
		if len(frame) != domain.FrameBytes {
			t.Fatalf("outbound frame length = %d", len(frame))
		}
		for _, b := range frame {
			if b != 0 {
				nonZeroOut = true
				break
			}
		}
	}
	if !nonZeroOut {
		t.Error("outbound audio was silent (real TTS conversion did not produce samples)")
	}
	if hangups != 1 {
		t.Errorf("hangup count = %d, want 1", hangups)
	}
	if audioAtHangup != len(sent) {
		t.Errorf("audio after hangup: sent at hangup %d, total %d", audioAtHangup, len(sent))
	}

	// Hygiene: no fake secret, patient value, outcome detail, or real endpoint appears in
	// the captured logs (failure messages name only the category, never the value).
	for category, value := range map[string]string{
		"API key":        integrationAPIKey,
		"patient name":   "Synthetic Patient",
		"patient reason": "Kontrolle",
		"outcome detail": "Termin bestätigt",
		"real endpoint":  "api.mistral.ai",
	} {
		if strings.Contains(h.logs.String(), value) {
			t.Errorf("%s leaked into logs", category)
		}
	}
}

func TestM0IntegrationOutcomeVariants(t *testing.T) {
	cases := map[string]struct {
		outcomeJSON string
		wantKind    domain.OutcomeKind
		wantReason  string
	}{
		"aborted": {
			outcomeJSON: `{"kind":"aborted","detail":"automatischer Testabbruch"}`,
			wantKind:    domain.OutcomeAborted,
		},
		"failed": {
			outcomeJSON: `{"kind":"failed","detail":"kein Termin","fail_reason":"rejected"}`,
			wantKind:    domain.OutcomeFailed,
			wantReason:  "rejected",
		},
		"callback_needed": {
			outcomeJSON: `{"kind":"callback_needed","detail":"Rückruf vereinbart","callback_at":"2027-01-16T10:00:00+01:00"}`,
			wantKind:    domain.OutcomeCallbackNeeded,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := func(n int, _ chatRequest) chatScript {
				if n == 1 {
					return chatScript{
						texts: []string{"Danke, auf Wiederhören."},
						calls: []wireCall{{id: "call_finish", name: "finish_call", args: tc.outcomeJSON}},
					}
				}
				return chatScript{texts: []string{"unexpected"}}
			}
			h := newHarness(t, script)
			final, err := h.run(t)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome == nil || final.Outcome.Kind != tc.wantKind {
				t.Fatalf("outcome = %+v, want %s", final.Outcome, tc.wantKind)
			}
			if final.Outcome.FailReason != tc.wantReason {
				t.Errorf("fail_reason = %q, want %q", final.Outcome.FailReason, tc.wantReason)
			}
			if final.Attempts != 1 {
				t.Errorf("attempts = %d", final.Attempts)
			}
			h.mistral.mu.Lock()
			results := len(h.mistral.toolResults)
			h.mistral.mu.Unlock()
			if results != 0 {
				t.Errorf("finish_call produced %d tool results", results)
			}
		})
	}
}

func TestM0IntegrationRemoteFailures(t *testing.T) {
	for _, reason := range []domain.EndReason{domain.EndBusy, domain.EndNoAnswer} {
		t.Run(string(reason), func(t *testing.T) {
			h := newHarness(t, func(int, chatRequest) chatScript { return chatScript{} })
			h.session.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventDialing, At: time.Now()}
			h.session.events <- domain.CallEvent{CallID: "call-integration", Kind: domain.EventRinging, At: time.Now()}
			h.session.end(reason)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			final, err := h.runner.Run(ctx, h.order.ID)
			if err != nil {
				t.Fatalf("remote outcome returned an operational error: %v", err)
			}
			if final.Outcome == nil || final.Outcome.Kind != domain.OutcomeFailed || final.Outcome.FailReason != string(reason) {
				t.Fatalf("outcome = %+v", final.Outcome)
			}
			h.mistral.mu.Lock()
			chats := h.mistral.chatCount
			h.mistral.mu.Unlock()
			if chats != 0 {
				t.Errorf("dialogue started for a never-answered call")
			}
		})
	}
}

// tonePCMFrame generates a stationary canonical sine tone for hold fixtures.
func tonePCMFrame(freq float64, amplitude int16) []byte {
	frame := make([]byte, domain.FrameBytes)
	for i := 0; i < domain.FrameBytes/2; i++ {
		v := int16(float64(amplitude) * math.Sin(2*math.Pi*freq*float64(i)/16000))
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(v))
	}
	return frame
}
