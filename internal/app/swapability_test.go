package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	dialogueopenai "github.com/artemidius/insteadly/internal/adapters/dialogue/openai"
	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/adapters/telephony/telnyx"
	coreagent "github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	corenotify "github.com/artemidius/insteadly/internal/core/notify"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/recorder"
	"github.com/artemidius/insteadly/internal/core/runner"
)

const (
	swapCallControl  = "v3:swap-call-control"
	swapPublicNumber = "+436641234567"
)

// swapTelnyxAPI is an in-repo stand-in for api.telnyx.com/v2. It records what the adapter
// sends and never contacts Telnyx.
type swapTelnyxAPI struct {
	mu         sync.Mutex
	webhookURL string
	streamURL  string
	streams    int
	hangups    int
	dtmf       string
}

func (a *swapTelnyxAPI) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer KEY_swap" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/v2/calls":
		var body struct {
			WebhookURL string `json:"webhook_url"`
			To         string `json:"to"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.webhookURL = body.WebhookURL
		a.mu.Unlock()
		writeSwapJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"call_control_id": swapCallControl, "call_leg_id": "leg", "call_session_id": "session",
		}})
	case strings.HasSuffix(r.URL.Path, "/actions/streaming_start"):
		var body struct {
			StreamURL string `json:"stream_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.streamURL = body.StreamURL
		a.streams++
		a.mu.Unlock()
		writeSwapJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	case strings.HasSuffix(r.URL.Path, "/actions/send_dtmf"):
		var body struct {
			Digits string `json:"digits"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.dtmf = body.Digits
		a.mu.Unlock()
		writeSwapJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	case strings.HasSuffix(r.URL.Path, "/actions/hangup"):
		a.mu.Lock()
		a.hangups++
		a.mu.Unlock()
		writeSwapJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"result": "ok"}})
	default:
		http.NotFound(w, r)
	}
}

// lockedBuffer is a concurrency-safe log sink: the app's adapters, runner, and notify
// worker all log from their own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeSwapJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// swapSTT is a deterministic SpeechToText fake: one final transcript per stream on Close,
// and a fixed batch transcript that records the raw PCM it was asked to transcribe.
type swapSTT struct {
	mu        sync.Mutex
	batchText string
	gotPCM    []byte
}

func (s *swapSTT) TranscribeStream(ctx context.Context, _ ports.StreamOptions) (ports.TranscriptionStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &swapSTTStream{results: make(chan ports.TranscriptionEvent, 1), text: "Ich brauche einen Termin."}, nil
}

func (s *swapSTT) TranscribeFile(ctx context.Context, r io.Reader, _ ports.BatchOptions) (domain.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return domain.Transcript{}, err
	}
	data, _ := io.ReadAll(r)
	s.mu.Lock()
	s.gotPCM = data
	s.mu.Unlock()
	return domain.Transcript{Text: s.batchText}, nil
}

func (s *swapSTT) batchAudio() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.gotPCM...)
}

type swapSTTStream struct {
	once    sync.Once
	results chan ports.TranscriptionEvent
	text    string
}

func (s *swapSTTStream) Feed(context.Context, domain.AudioChunk) error { return nil }

func (s *swapSTTStream) Results() <-chan ports.TranscriptionEvent { return s.results }

func (s *swapSTTStream) Close(context.Context) error {
	s.once.Do(func() {
		s.results <- ports.TranscriptionEvent{Text: s.text, Final: true, At: time.Now()}
		close(s.results)
	})
	return nil
}

// swapTTS is a deterministic TextToSpeech fake: each FeedText emits one canonical frame.
type swapTTS struct{}

func (swapTTS) Speak(ctx context.Context, _ ports.SpeakOptions) (ports.SpeechStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &swapTTSStream{audio: make(chan domain.AudioChunk, 64), done: make(chan struct{})}, nil
}

type swapTTSStream struct {
	audio     chan domain.AudioChunk
	done      chan struct{}
	sendMu    sync.Mutex
	closeOnce sync.Once
}

func (s *swapTTSStream) Audio() <-chan domain.AudioChunk { return s.audio }

func (s *swapTTSStream) FeedText(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	select {
	case s.audio <- domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}:
		return nil
	case <-s.done:
		return errors.New("swap tts: closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *swapTTSStream) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.sendMu.Lock()
		close(s.audio)
		s.sendMu.Unlock()
	})
}

func (s *swapTTSStream) Abort(context.Context) error { s.close(); return nil }

func (s *swapTTSStream) Close(context.Context) error { s.close(); return nil }

// openAIScript is the M0 booked conversation served over OpenAI's SSE shape. Request 1
// proposes a slot, request 2 confirms it, request 3 finalizes the booking.
func openAIScript(t *testing.T) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	propose := `{"start":"2027-01-15T09:00:00+01:00","end":"2027-01-15T09:30:00+01:00","doctor":"Dr. A","practice":"Praxis A"}`
	confirm := `{"start":"2027-01-15T09:00:00+01:00","end":"2027-01-15T09:30:00+01:00","doctor":"Dr. A","practice":"Praxis A","confirmation_code":"TEST-123"}`
	booked := `{"kind":"booked","detail":"Termin bestätigt","booked_slot":{"date_time":"2027-01-15T09:00:00+01:00"}}`

	sseToolCall := func(id, name, args string) string {
		chunk, _ := json.Marshal(map[string]any{
			"id": "chat_1", "object": "chat.completion.chunk",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}}},
				"finish_reason": nil,
			}},
		})
		done, _ := json.Marshal(map[string]any{
			"id": "chat_1", "object": "chat.completion.chunk",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		})
		return "data: " + string(chunk) + "\n\ndata: " + string(done) + "\n\ndata: [DONE]\n\n"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-swap" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			_, _ = io.WriteString(w, sseToolCall("call_propose", "propose_slot", propose))
		case 2:
			_, _ = io.WriteString(w, sseToolCall("call_confirm", "confirm_booking", confirm))
		case 3:
			_, _ = io.WriteString(w, sseToolCall("call_finish", "finish_call", booked))
		default:
			t.Errorf("unexpected OpenAI request %d", n)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}
}

func swapTestOrder() domain.Order {
	return domain.Order{
		Status:  domain.OrderScheduled,
		Clinic:  domain.Clinic{Name: "Praxis Swap", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Synthetic Swap Patient", Reason: "Kontrolle"},
	}
}

// canonicalSpeechFrames builds the same deterministic non-stationary fixture the M0
// integration harness uses: 20 voiced frames followed by the 700 ms hangover.
func canonicalSpeechFrames() [][]byte {
	freqs := [6]float64{220, 730, 1150, 1750, 2650, 3300}
	out := make([][]byte, 0, 55)
	for seed := 0; seed < 20; seed++ {
		frame := make([]byte, domain.FrameBytes)
		for i := 0; i < domain.FrameBytes/2; i++ {
			var v float64
			for k := range freqs {
				amp := 0.10 + 0.08*math.Sin(float64(seed*7+k*3))
				phase := float64(seed*11+k*5) * 0.37
				v += amp * math.Sin(2*math.Pi*freqs[k]*float64(i)/16000+phase)
			}
			binary.LittleEndian.PutUint16(frame[2*i:], uint16(int16(v*18000)))
		}
		out = append(out, frame)
	}
	for i := 0; i < 35; i++ {
		out = append(out, make([]byte, domain.FrameBytes))
	}
	return out
}

// canonicalToULawFrames decimates each canonical 20 ms frame to 8 kHz and G.711-encodes
// it, exactly what a real Telnyx media stream would deliver on the inbound leg.
func canonicalToULawFrames(frames [][]byte) [][]byte {
	out := make([][]byte, 0, len(frames))
	for _, pcm := range frames {
		frame := make([]byte, telnyxFrameBytesForTest)
		for i := range frame {
			sample := int16(binary.LittleEndian.Uint16(pcm[4*i:]))
			frame[i] = encodeULawTest(sample)
		}
		out = append(out, frame)
	}
	return out
}

// telnyxFrameBytesForTest mirrors the adapter's 160-byte 8 kHz µ-law frame.
const telnyxFrameBytesForTest = 160

// encodeULawTest is the standard G.711 encoder, copied into the test so the fixture does
// not depend on an unexported adapter helper.
func encodeULawTest(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	v := int32(sample)
	var sign byte
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > clip {
		v = clip
	}
	v += bias
	exponent := 7
	for mask := int32(0x4000); exponent > 0 && v&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte((v >> (exponent + 3)) & 0x0f)
	return ^(sign | byte(exponent<<4) | mantissa)
}

func waitSwap(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSwapabilityTelnyxOpenAIWithRecordingAndNotify is the C7 swapability proof: the real
// Telnyx telephony adapter and the real OpenAI dialogue adapter, driven against in-repo
// fake HTTP/SSE servers and a loopback media-stream WebSocket, with recording on and a log
// notifier. No Twilio adapter, core runner/agent change, Telnyx/OpenAI account, or key is
// involved.
func TestSwapabilityTelnyxOpenAIWithRecordingAndNotify(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	api := &swapTelnyxAPI{}
	apiSrv := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(apiSrv.Close)

	var tel *telnyx.Adapter
	mount := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tel.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(mount.Close)
	var err error
	tel, err = telnyx.New(telnyx.Config{
		APIKey:        "KEY_swap",
		ConnectionID:  "conn_swap",
		FromNumber:    "+15551234567",
		PublicBaseURL: mount.URL,
		APIBaseURL:    apiSrv.URL,
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("telnyx.New: %v", err)
	}

	dialogueSrv := httptest.NewServer(openAIScript(t))
	t.Cleanup(dialogueSrv.Close)
	dialogue, err := dialogueopenai.New(dialogueopenai.Config{
		APIKey: "sk-swap", BaseURL: dialogueSrv.URL, Model: "gpt-4o-mini", Logger: logger,
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}

	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	order := swapTestOrder()
	created, err := store.Orders().Create(context.Background(), order)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Orders().Update(context.Background(), created.ID, func(o *domain.Order) error {
		if err := o.Transition(domain.OrderPaid); err != nil {
			return err
		}
		return o.Transition(domain.OrderScheduled)
	}); err != nil {
		t.Fatalf("seed scheduled: %v", err)
	}

	stt := &swapSTT{batchText: "Ich brauche einen Termin."}
	tts := swapTTS{}
	ag, err := coreagent.New(coreagent.Config{
		STT: stt, TTS: &tts, Dialogue: dialogue, Orders: store.Orders(), Logger: logger,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	rec := recorder.New(stt, store.Blobs(), recorder.Policy{
		Recording: true, Audio: true, Transcript: true,
	}, logger)
	hub := events.NewHub(64)
	run, err := runner.New(runner.Config{
		Telephony:        tel,
		Orders:           store.Orders(),
		Agent:            ag,
		Logger:           logger,
		Events:           hub,
		Recorder:         rec,
		CallTimeout:      30 * time.Second,
		CleanupTimeout:   3 * time.Second,
		ReadinessTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	notifier, err := buildNotifier(config.Notify{
		Provider: "log",
		Webhook:  config.NotifyWebhook{Channel: "email"},
	}, logger)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	worker, err := corenotify.New(corenotify.Config{
		Events: hub, Orders: store.Orders(), Notifier: notifier, Logger: logger,
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}

	application := &App{
		Config:       config.Config{Notify: config.Notify{Provider: "log"}},
		Storage:      store,
		Telephony:    tel,
		STT:          stt,
		TTS:          &tts,
		Dialogue:     dialogue,
		Notify:       notifier,
		Log:          logger,
		Agent:        ag,
		Runner:       run,
		Events:       hub,
		notifyWorker: worker,
	}
	application.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := application.Close(ctx); err != nil {
			t.Errorf("app.Close: %v", err)
		}
	})

	done := make(chan struct {
		order domain.Order
		err   error
	}, 1)
	go func() {
		final, err := run.Run(context.Background(), created.ID)
		done <- struct {
			order domain.Order
			err   error
		}{final, err}
	}()

	// The runner dials through the fake REST API; its webhook URL carries the call id.
	waitSwap(t, "telnyx dial", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.webhookURL != ""
	})
	api.mu.Lock()
	webhookURL := api.webhookURL
	api.mu.Unlock()
	callID := path.Base(webhookURL)

	// Telnyx: call.answered. The adapter responds with streaming_start.
	answerBody, _ := json.Marshal(map[string]any{"data": map[string]any{
		"event_type":  "call.answered",
		"id":          "evt-answer",
		"record_type": "event",
		"payload": map[string]any{
			"call_control_id": swapCallControl,
			"connection_id":   "conn_swap",
		},
	}})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(answerBody))
	if err != nil {
		t.Fatalf("post call.answered: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("call.answered = %d", resp.StatusCode)
	}
	waitSwap(t, "streaming_start", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.streams == 1
	})

	// The test plays Telnyx: open the media stream and echo playback marks.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wsURL := "ws" + strings.TrimPrefix(mount.URL, "http") + "/webhooks/telephony/telnyx/stream/" + callID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	var writeMu sync.Mutex
	write := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = wsjson.Write(ctx, conn, v)
	}
	write(map[string]any{"event": "connected", "version": "1.0.0"})
	write(map[string]any{
		"event": "start", "stream_id": "stream-swap",
		"start": map[string]any{
			"call_control_id": swapCallControl,
			"media_format":    map[string]any{"encoding": "PCMU", "sample_rate": 8000, "channels": 1},
		},
	})

	var marksMu sync.Mutex
	marks := 0
	mediaFrames := 0
	go func() {
		for {
			var msg map[string]any
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			switch msg["event"] {
			case "media":
				marksMu.Lock()
				mediaFrames++
				marksMu.Unlock()
			case "mark":
				mark, _ := msg["mark"].(map[string]any)
				name, _ := mark["name"].(string)
				write(map[string]any{"event": "mark", "mark": map[string]any{"name": name}})
				marksMu.Lock()
				marks++
				marksMu.Unlock()
			case "clear":
				// barge-in clear: nothing to do for the fixture
			}
		}
	}()

	waitSwap(t, "greeting playback marks", func() bool {
		marksMu.Lock()
		defer marksMu.Unlock()
		return marks >= 2
	})

	for _, frame := range canonicalToULawFrames(canonicalSpeechFrames()) {
		write(map[string]any{
			"event": "media",
			"media": map[string]any{
				"track": "inbound", "chunk": "1", "timestamp": "20",
				"payload": base64.StdEncoding.EncodeToString(frame),
			},
		})
	}

	var result struct {
		order domain.Order
		err   error
	}
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not return")
	}
	if result.err != nil {
		t.Fatalf("runner.Run: %v", result.err)
	}
	if result.order.Status != domain.OrderFinished {
		t.Fatalf("status = %q, want finished", result.order.Status)
	}
	if result.order.Outcome == nil || result.order.Outcome.Kind != domain.OutcomeBooked {
		t.Fatalf("outcome = %+v, want booked", result.order.Outcome)
	}

	// Recording blobs were persisted through ports.BlobStore.
	inKey := domain.BlobKey{OrderID: created.ID, Kind: domain.BlobKindRecording, Name: callID + "-inbound.wav"}
	outKey := domain.BlobKey{OrderID: created.ID, Kind: domain.BlobKindRecording, Name: callID + "-outbound.wav"}
	txKey := domain.BlobKey{OrderID: created.ID, Kind: domain.BlobKindTranscript, Name: callID + "-transcript.txt"}

	readBlob := func(key domain.BlobKey) []byte {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rc, err := store.Blobs().Open(context.Background(), key)
			if err == nil {
				defer rc.Close()
				data, _ := io.ReadAll(rc)
				return data
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("blob %s/%s was never persisted", key.Kind, key.Name)
		return nil
	}
	inWAV := readBlob(inKey)
	outWAV := readBlob(outKey)
	tx := readBlob(txKey)
	if string(inWAV[0:4]) != "RIFF" || string(outWAV[0:4]) != "RIFF" {
		t.Fatal("recorded WAVs do not carry a RIFF header")
	}
	if got := binary.LittleEndian.Uint32(inWAV[24:28]); got != domain.SampleRate {
		t.Fatalf("inbound recording sample rate = %d, want %d", got, domain.SampleRate)
	}
	if len(inWAV) <= 44 {
		t.Fatal("inbound recording has no audio payload")
	}
	if !bytes.Contains(tx, []byte("insteadly transcript")) ||
		!bytes.Contains(tx, []byte("order="+string(created.ID))) ||
		!bytes.Contains(tx, []byte("call="+callID)) ||
		!bytes.Contains(tx, []byte("clinic=Praxis Swap")) ||
		!bytes.Contains(tx, []byte("Ich brauche einen Termin.")) {
		t.Fatalf("transcript blob = %q", tx)
	}
	if len(stt.batchAudio()) == 0 {
		t.Fatal("TranscribeFile received no inbound PCM")
	}

	// The notifier worker turned the terminal outcome into a logged notification.
	waitSwap(t, "notification", func() bool {
		out := logs.String()
		return strings.Contains(out, "notification") &&
			strings.Contains(out, string(created.ID)) &&
			strings.Contains(out, "Termin bestätigt")
	})
	if application.Notify == nil {
		t.Fatal("app.Notify is nil with notify.provider=log")
	}
	if api.hangups == 0 {
		t.Error("adapter never asked Telnyx to hang up")
	}
	marksMu.Lock()
	sawMedia := mediaFrames > 0
	marksMu.Unlock()
	if !sawMedia {
		t.Error("no outbound media reached the fake Telnyx stream")
	}
}
