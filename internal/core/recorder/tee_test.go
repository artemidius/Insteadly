package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// --- fakes -----------------------------------------------------------------------

type fakeSession struct {
	id      domain.CallID
	events  chan domain.CallEvent
	inbound chan domain.AudioChunk

	mu      sync.Mutex
	sent    [][]byte
	dtmf    []string
	clears  int
	marks   []string
	hangups int
	ready   chan struct{}
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		id:      "call-0123456789abcdef0123456789ab",
		events:  make(chan domain.CallEvent, 8),
		inbound: make(chan domain.AudioChunk, 16),
		ready:   make(chan struct{}),
	}
}

func (s *fakeSession) ID() domain.CallID               { return s.id }
func (s *fakeSession) Events() <-chan domain.CallEvent { return s.events }
func (s *fakeSession) Inbound() <-chan domain.AudioChunk {
	return s.inbound
}

func (s *fakeSession) SendAudio(_ context.Context, chunk domain.AudioChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, append([]byte(nil), chunk.Data...))
	return nil
}

func (s *fakeSession) SendDTMF(_ context.Context, digits string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dtmf = append(s.dtmf, digits)
	return nil
}

func (s *fakeSession) ClearAudio(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
	return nil
}

func (s *fakeSession) MarkPlayback(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, id)
	return nil
}

func (s *fakeSession) Hangup(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangups++
	return nil
}

// readySession exposes the optional StreamReady diagnostic.
type readySession struct{ *fakeSession }

func (r readySession) StreamReady() <-chan struct{} { return r.ready }

type memBlobs struct {
	mu    sync.Mutex
	blobs map[domain.BlobKey][]byte
	puts  int
}

func newMemBlobs() *memBlobs {
	return &memBlobs{blobs: make(map[domain.BlobKey][]byte)}
}

func (m *memBlobs) Put(_ context.Context, key domain.BlobKey, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[key] = data
	m.puts++
	return nil
}

func (m *memBlobs) Open(context.Context, domain.BlobKey) (io.ReadCloser, error) {
	return nil, ports.ErrNotFound
}

func (m *memBlobs) Delete(context.Context, domain.BlobKey) error { return nil }

func (m *memBlobs) get(kind, name string, orderID domain.OrderID) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[domain.BlobKey{OrderID: orderID, Kind: kind, Name: name}]
	return data, ok
}

func (m *memBlobs) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

type fakeSTT struct {
	mu       sync.Mutex
	called   int
	gotAudio []byte
	gotOpts  ports.BatchOptions
	text     string
	err      error
}

func (s *fakeSTT) TranscribeStream(context.Context, ports.StreamOptions) (ports.TranscriptionStream, error) {
	return nil, nil
}

func (s *fakeSTT) TranscribeFile(_ context.Context, r io.Reader, opts ports.BatchOptions) (domain.Transcript, error) {
	data, _ := io.ReadAll(r)
	s.mu.Lock()
	s.called++
	s.gotAudio = data
	s.gotOpts = opts
	s.mu.Unlock()
	if s.err != nil {
		return domain.Transcript{}, s.err
	}
	return domain.Transcript{Text: s.text}, nil
}

func testOrder() domain.Order {
	return domain.Order{
		ID:     "0123456789abcdef0123456789abcdef",
		Clinic: domain.Clinic{Name: "Praxis Test", Phone: "+436641234567"},
	}
}

func waitFinalized(t *testing.T, session ports.CallSession) *teeSession {
	t.Helper()
	tee, ok := session.(*teeSession)
	if !ok {
		t.Fatalf("Wrap returned %T, want *teeSession", session)
	}
	select {
	case <-tee.finalized:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for finalize")
	}
	return tee
}

func drainInbound(ch <-chan domain.AudioChunk) [][]byte {
	var out [][]byte
	for chunk := range ch {
		out = append(out, chunk.Data)
	}
	return out
}

// --- tests -----------------------------------------------------------------------

func TestWrapDefaultsOffReturnsOriginalSession(t *testing.T) {
	inner := newFakeSession()
	rec := New(&fakeSTT{}, newMemBlobs(), Policy{}, nil)
	if got := rec.Wrap(inner, testOrder()); got != inner {
		t.Fatalf("Wrap with all flags off returned %T, want the identical session", got)
	}
	// recording on but neither artifact selected also captures nothing.
	rec = New(&fakeSTT{}, newMemBlobs(), Policy{Recording: true}, nil)
	if got := rec.Wrap(inner, testOrder()); got != inner {
		t.Fatalf("Wrap with no artifact selected returned %T, want the identical session", got)
	}
}

func TestWrapCapturesBothDirectionsAndForwardsUnchanged(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	stt := &fakeSTT{text: "transcript"}
	rec := New(stt, blobs, Policy{Recording: true, Audio: true, Transcript: true}, nil)
	order := testOrder()
	wrapped := rec.Wrap(inner, order)

	in1 := []byte{1, 2, 3, 4}
	in2 := []byte{5, 6}
	inner.inbound <- domain.AudioChunk{Data: in1}
	inner.inbound <- domain.AudioChunk{Data: in2}

	// The forwarded channel is unchanged, in order.
	for _, want := range [][]byte{in1, in2} {
		select {
		case got := <-wrapped.Inbound():
			if !bytes.Equal(got.Data, want) {
				t.Fatalf("forwarded inbound = %v, want %v", got.Data, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for forwarded inbound")
		}
	}

	out := []byte{9, 9, 9, 9}
	if err := wrapped.SendAudio(context.Background(), domain.AudioChunk{Data: out}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if err := wrapped.SendAudio(context.Background(), domain.AudioChunk{}); err != nil {
		t.Fatalf("SendAudio(empty): %v", err)
	}

	// Forwarded calls still reach the inner session.
	if err := wrapped.SendDTMF(context.Background(), "1#"); err != nil {
		t.Fatalf("SendDTMF: %v", err)
	}
	if err := wrapped.ClearAudio(context.Background()); err != nil {
		t.Fatalf("ClearAudio: %v", err)
	}
	if err := wrapped.MarkPlayback(context.Background(), "mark-1"); err != nil {
		t.Fatalf("MarkPlayback: %v", err)
	}

	// End the call by closing the adapter channel; the relay finalizes.
	close(inner.inbound)
	waitFinalized(t, wrapped)

	if len(inner.sent) != 2 || !bytes.Equal(inner.sent[0], out) {
		t.Fatalf("inner sent audio = %v", inner.sent)
	}
	if len(inner.dtmf) != 1 || inner.dtmf[0] != "1#" {
		t.Errorf("inner dtmf = %v", inner.dtmf)
	}
	if inner.clears != 1 || len(inner.marks) != 1 {
		t.Errorf("inner clears=%d marks=%v", inner.clears, inner.marks)
	}

	callID := string(inner.ID())
	inWAV, ok := blobs.get(domain.BlobKindRecording, callID+inboundSuffix, order.ID)
	if !ok {
		t.Fatal("inbound recording blob missing")
	}
	if len(inWAV) != wavHeaderSize+len(in1)+len(in2) {
		t.Fatalf("inbound WAV len = %d", len(inWAV))
	}
	if !bytes.Equal(inWAV[wavHeaderSize:], append(append([]byte(nil), in1...), in2...)) {
		t.Fatal("inbound WAV payload differs from captured audio")
	}

	outWAV, ok := blobs.get(domain.BlobKindRecording, callID+outboundSuffix, order.ID)
	if !ok {
		t.Fatal("outbound recording blob missing")
	}
	if !bytes.Equal(outWAV[wavHeaderSize:], out) {
		t.Fatal("outbound WAV payload differs (empty chunk must not be captured)")
	}

	txBlob, ok := blobs.get(domain.BlobKindTranscript, callID+transcriptSuffix, order.ID)
	if !ok {
		t.Fatal("transcript blob missing")
	}
	text := string(txBlob)
	if !bytes.Contains(txBlob, []byte("insteadly transcript\norder="+string(order.ID)+"\ncall="+callID+"\n")) {
		t.Errorf("transcript header = %q", text)
	}
	if !bytes.Contains(txBlob, []byte("clinic=Praxis Test")) || !bytes.Contains(txBlob, []byte("---\ntranscript")) {
		t.Errorf("transcript body = %q", text)
	}
	if !bytes.Equal(stt.gotAudio, append(append([]byte(nil), in1...), in2...)) {
		t.Errorf("TranscribeFile received %v, want raw inbound PCM", stt.gotAudio)
	}
	if stt.gotOpts.Language != "de" || stt.gotOpts.Timestamps || stt.gotOpts.Diarization {
		t.Errorf("batch options = %+v", stt.gotOpts)
	}
}

func TestWrapTranscriptOnlyDiscardsWAV(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	stt := &fakeSTT{text: "nur transcript"}
	wrapped := New(stt, blobs, Policy{Recording: true, Transcript: true}, nil).Wrap(inner, testOrder())

	inner.inbound <- domain.AudioChunk{Data: []byte{1, 2}}
	_ = wrapped.SendAudio(context.Background(), domain.AudioChunk{Data: []byte{3, 4}})
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	waitFinalized(t, wrapped)

	callID := string(inner.ID())
	if _, ok := blobs.get(domain.BlobKindRecording, callID+inboundSuffix, testOrder().ID); ok {
		t.Error("transcript-only must not persist a WAV")
	}
	if _, ok := blobs.get(domain.BlobKindTranscript, callID+transcriptSuffix, testOrder().ID); !ok {
		t.Error("transcript blob missing")
	}
	if stt.called != 1 {
		t.Fatalf("TranscribeFile calls = %d, want 1", stt.called)
	}
}

func TestWrapWAVOnlySkipsTranscription(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	stt := &fakeSTT{text: "should not run"}
	wrapped := New(stt, blobs, Policy{Recording: true, Audio: true}, nil).Wrap(inner, testOrder())

	inner.inbound <- domain.AudioChunk{Data: []byte{1, 2}}
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	waitFinalized(t, wrapped)

	callID := string(inner.ID())
	if _, ok := blobs.get(domain.BlobKindRecording, callID+inboundSuffix, testOrder().ID); !ok {
		t.Error("inbound WAV missing")
	}
	if _, ok := blobs.get(domain.BlobKindRecording, callID+outboundSuffix, testOrder().ID); !ok {
		t.Error("outbound WAV missing (written even when empty)")
	}
	if _, ok := blobs.get(domain.BlobKindTranscript, callID+transcriptSuffix, testOrder().ID); ok {
		t.Error("WAV-only must not persist a transcript")
	}
	if stt.called != 0 {
		t.Errorf("TranscribeFile called %d times, want 0", stt.called)
	}
}

func TestFinalizeIsIdempotent(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	wrapped := New(&fakeSTT{text: "x"}, blobs, Policy{Recording: true, Audio: true}, nil).Wrap(inner, testOrder())

	// Hangup triggers finalize, then the adapter close must not finalize a second time.
	if err := wrapped.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	close(inner.inbound)
	waitFinalized(t, wrapped)
	if got := blobs.putCount(); got != 2 {
		t.Fatalf("Put calls = %d, want 2 (one per direction, exactly once)", got)
	}
}

// Hangup is the fallback finalize trigger for a caller that stops draining Inbound.
func TestHangupFinalizesWithoutInboundClose(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	wrapped := New(&fakeSTT{text: "x"}, blobs, Policy{Recording: true, Audio: true}, nil).Wrap(inner, testOrder())

	inner.inbound <- domain.AudioChunk{Data: []byte{1, 2}}
	// Consume the forwarded chunk so the relay is not stuck on delivery.
	<-wrapped.Inbound()
	if err := wrapped.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	waitFinalized(t, wrapped)
	if _, ok := blobs.get(domain.BlobKindRecording, string(inner.ID())+inboundSuffix, testOrder().ID); !ok {
		t.Fatal("Hangup did not finalize the inbound recording")
	}
	// The wrapper must still be usable for late adapter/runner calls.
	if _, ok := <-wrapped.Inbound(); ok {
		t.Fatal("inbound channel must close after finalize")
	}
}

func TestFinalizeBestEffortOnSTTError(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	wrapped := New(&fakeSTT{err: io.ErrUnexpectedEOF}, blobs, Policy{Recording: true, Audio: true, Transcript: true}, nil).Wrap(inner, testOrder())

	inner.inbound <- domain.AudioChunk{Data: []byte{1, 2}}
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	waitFinalized(t, wrapped)

	// WAVs are still persisted; only the transcript is skipped.
	callID := string(inner.ID())
	if _, ok := blobs.get(domain.BlobKindRecording, callID+inboundSuffix, testOrder().ID); !ok {
		t.Error("WAV must survive a transcript failure")
	}
	if _, ok := blobs.get(domain.BlobKindTranscript, callID+transcriptSuffix, testOrder().ID); ok {
		t.Error("failed transcript must not be persisted")
	}
}

func TestFinalizeBestEffortOnBlobError(t *testing.T) {
	inner := newFakeSession()
	blobs := &failingBlobs{memBlobs: newMemBlobs()}
	wrapped := New(&fakeSTT{}, blobs, Policy{Recording: true, Audio: true}, nil).Wrap(inner, testOrder())
	inner.inbound <- domain.AudioChunk{Data: []byte{1}}
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	// Must not panic or block; finalize logs and completes.
	waitFinalized(t, wrapped)
}

type failingBlobs struct{ *memBlobs }

func (f *failingBlobs) Put(context.Context, domain.BlobKey, io.Reader) error {
	return io.ErrClosedPipe
}

func TestStreamReadyIsForwarded(t *testing.T) {
	inner := &fakeSession{
		id:      "call-ready",
		events:  make(chan domain.CallEvent, 1),
		inbound: make(chan domain.AudioChunk),
		ready:   make(chan struct{}),
	}
	wrapped := New(&fakeSTT{}, newMemBlobs(), Policy{Recording: true, Audio: true}, nil).Wrap(readySession{fakeSession: inner}, testOrder())
	withReady, ok := wrapped.(interface{ StreamReady() <-chan struct{} })
	if !ok {
		t.Fatal("wrapped session does not expose StreamReady")
	}
	if withReady.StreamReady() != inner.ready {
		t.Fatal("StreamReady channel was not forwarded")
	}

	plain := newFakeSession()
	wrapped = New(&fakeSTT{}, newMemBlobs(), Policy{Recording: true, Audio: true}, nil).Wrap(plain, testOrder())
	if got := wrapped.(interface{ StreamReady() <-chan struct{} }).StreamReady(); got != nil {
		t.Fatal("StreamReady must be nil when the underlying session has none")
	}
}

func TestWrapPreservesPerOrderOverride(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	rec := New(&fakeSTT{text: "x"}, blobs, Policy{}, nil) // global off
	order := testOrder()
	yes := true
	order.Recording = &domain.RecordingFlags{Recording: &yes, Audio: &yes}
	wrapped := rec.Wrap(inner, order)
	if _, ok := wrapped.(*teeSession); !ok {
		t.Fatalf("per-order override did not enable capture: %T", wrapped)
	}
	inner.inbound <- domain.AudioChunk{Data: []byte{1}}
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	waitFinalized(t, wrapped)
	if _, ok := blobs.get(domain.BlobKindRecording, string(inner.ID())+inboundSuffix, order.ID); !ok {
		t.Fatal("per-order override recording missing")
	}
}

func TestTranscriptHeaderCarriesKnownOutcome(t *testing.T) {
	order := testOrder()
	order.Outcome = &domain.Outcome{Kind: domain.OutcomeBooked, Detail: "ok", BookedSlot: &domain.BookedSlot{DateTime: time.Now()}}
	tee := &teeSession{inner: newFakeSession(), order: order}
	text := tee.transcriptText(domain.Transcript{Text: "hallo"})
	want := "insteadly transcript\norder=" + string(order.ID) + "\ncall=" + string(tee.inner.ID()) +
		"\nclinic=Praxis Test\noutcome=booked\n---\nhallo"
	if text != want {
		t.Fatalf("transcript text =\n%q\nwant\n%q", text, want)
	}
}

func TestTranscriptHeaderEmptyOutcomeAtWrapTime(t *testing.T) {
	tee := &teeSession{inner: newFakeSession(), order: testOrder()}
	text := tee.transcriptText(domain.Transcript{Text: "hallo"})
	if !bytes.Contains([]byte(text), []byte("\noutcome=\n")) {
		t.Fatalf("empty outcome must be an empty field: %q", text)
	}
}

func TestWAVHeaderValuesFromCapture(t *testing.T) {
	inner := newFakeSession()
	blobs := newMemBlobs()
	wrapped := New(&fakeSTT{}, blobs, Policy{Recording: true, Audio: true}, nil).Wrap(inner, testOrder())
	inner.inbound <- domain.AudioChunk{Data: make([]byte, domain.FrameBytes)}
	close(inner.inbound)
	drainInbound(wrapped.Inbound())
	waitFinalized(t, wrapped)

	data, ok := blobs.get(domain.BlobKindRecording, string(inner.ID())+inboundSuffix, testOrder().ID)
	if !ok {
		t.Fatal("inbound WAV missing")
	}
	if got := binary.LittleEndian.Uint32(data[24:28]); got != domain.SampleRate {
		t.Fatalf("sample rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(data[40:44]); got != domain.FrameBytes {
		t.Fatalf("data size = %d, want %d", got, domain.FrameBytes)
	}
}
