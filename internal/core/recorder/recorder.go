// Package recorder captures one call's audio and, on finalize, persists the WAV and/or
// the post-call transcript through ports.BlobStore, gated by the resolved recording
// flags. It is core: it imports only domain, ports, and stdlib — never an adapter.
//
// The recording flags already exist (config.Policy holds the global defaults and
// domain.Order.Recording the per-order overrides); C7 makes them effective. With every
// flag off, Wrap returns the dialed session unchanged, spawns no goroutine, and writes
// no bytes, so default behavior is byte-for-byte the pre-C7 behavior.
package recorder

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Artifact suffixes are appended to the call id in each blob name.
const (
	inboundSuffix    = "-inbound.wav"
	outboundSuffix   = "-outbound.wav"
	transcriptSuffix = "-transcript.txt"

	// inboundQueue bounds the forwarded inbound channel. It mirrors the adapter's own
	// bounded inbound buffer so a slow agent sees the same backpressure shape.
	inboundQueue = 100

	// finalizeTimeout bounds post-call transcription + blob writes. Finalize runs after
	// the call, so it must not inherit a canceled call context but must still terminate.
	finalizeTimeout = 30 * time.Second

	// maxCaptureBytes caps one direction's in-memory capture (64 MiB ≈ 35 minutes of
	// canonical PCM16). The adapters already cap a call at 30 minutes; this is a memory
	// safety net, not a product limit.
	maxCaptureBytes = 64 << 20
)

// Policy is the global recording defaults, copied from config.Policy by the composition
// root. The recorder never imports internal/core/config.
type Policy struct {
	Recording  bool
	Transcript bool
	Audio      bool
}

// Decision is the resolved per-order recording policy. Capture means "capture audio in
// memory" (required for either artifact); WAV and Transcript are the two
// storage-boundary decisions.
type Decision struct {
	Capture    bool
	WAV        bool
	Transcript bool
}

// Resolve merges the order's per-order overrides over the global defaults.
// recording is the master gate: recording=false => Capture/WAV/Transcript are all false.
func Resolve(global Policy, overrides *domain.RecordingFlags) Decision {
	recording := global.Recording
	if overrides != nil && overrides.Recording != nil {
		recording = *overrides.Recording
	}
	audio := global.Audio
	if overrides != nil && overrides.Audio != nil {
		audio = *overrides.Audio
	}
	transcript := global.Transcript
	if overrides != nil && overrides.Transcript != nil {
		transcript = *overrides.Transcript
	}
	d := Decision{}
	if !recording {
		return d
	}
	d.Capture = audio || transcript
	d.WAV = audio
	d.Transcript = transcript
	return d
}

// Recorder is the seam the runner consumes. The composition root constructs the concrete
// *RecorderImpl; the runner only sees this interface so it stays testable with a nil or
// fake recorder.
type Recorder interface {
	// Wrap returns a teeing ports.CallSession that captures this order's inbound and
	// outbound canonical audio when the resolved decision says to, or the original
	// session unchanged (and no goroutines) otherwise. It forwards every
	// ports.CallSession method, including StreamReady when the underlying session
	// exposes it.
	Wrap(session ports.CallSession, order domain.Order) ports.CallSession
}

// RecorderImpl is the concrete recorder. stt is used only for TranscribeFile (post-call
// batch transcription); blobs is the storage boundary; log may be nil.
type RecorderImpl struct {
	stt    ports.SpeechToText
	blobs  ports.BlobStore
	global Policy
	log    *slog.Logger
}

var _ Recorder = (*RecorderImpl)(nil)

// New builds the concrete recorder. A nil logger discards output.
func New(stt ports.SpeechToText, blobs ports.BlobStore, global Policy, log *slog.Logger) *RecorderImpl {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &RecorderImpl{stt: stt, blobs: blobs, global: global, log: log}
}

// Wrap returns the original session when nothing is captured (the default-off path) or a
// teeing wrapper that records both directions and finalizes on call end.
func (r *RecorderImpl) Wrap(session ports.CallSession, order domain.Order) ports.CallSession {
	decision := Resolve(r.global, order.Recording)
	if !decision.Capture || session == nil {
		return session
	}
	t := &teeSession{
		inner:     session,
		rec:       r,
		order:     order,
		decision:  decision,
		out:       make(chan domain.AudioChunk, inboundQueue),
		done:      make(chan struct{}),
		finalized: make(chan struct{}),
	}
	go t.relay()
	return t
}

// teeSession is the wrapping ports.CallSession: it captures inbound audio in a relay
// goroutine and outbound audio in SendAudio, forwarding everything unchanged.
type teeSession struct {
	inner    ports.CallSession
	rec      *RecorderImpl
	order    domain.Order
	decision Decision

	out chan domain.AudioChunk
	// done closes when finalize has finished; the relay stops on it.
	done chan struct{}
	// finalized closes after finalize's best-effort work (tests wait on it).
	finalized chan struct{}

	inMu       sync.Mutex
	inPCM      []byte
	inCap      bool // cap warning already logged
	finalizing sync.Once

	outMu  sync.Mutex
	outPCM []byte
	outCap bool
}

var _ ports.CallSession = (*teeSession)(nil)

func (t *teeSession) ID() domain.CallID               { return t.inner.ID() }
func (t *teeSession) Events() <-chan domain.CallEvent { return t.inner.Events() }
func (t *teeSession) Inbound() <-chan domain.AudioChunk {
	return t.out
}

// StreamReady forwards the adapter-specific readiness diagnostic when the underlying
// session exposes it. Returning nil otherwise is exactly what the runner treats as "no
// readiness wait".
func (t *teeSession) StreamReady() <-chan struct{} {
	withReady, ok := t.inner.(interface{ StreamReady() <-chan struct{} })
	if !ok {
		return nil
	}
	return withReady.StreamReady()
}

// SendAudio captures the outbound chunk, then forwards it to the real session.
func (t *teeSession) SendAudio(ctx context.Context, chunk domain.AudioChunk) error {
	if len(chunk.Data) > 0 {
		t.outMu.Lock()
		t.outPCM, t.outCap = appendCapped(t.outPCM, chunk.Data, t.outCap, t)
		t.outMu.Unlock()
	}
	return t.inner.SendAudio(ctx, chunk)
}

func (t *teeSession) SendDTMF(ctx context.Context, digits string) error {
	return t.inner.SendDTMF(ctx, digits)
}

func (t *teeSession) ClearAudio(ctx context.Context) error {
	return t.inner.ClearAudio(ctx)
}

func (t *teeSession) MarkPlayback(ctx context.Context, playbackID string) error {
	return t.inner.MarkPlayback(ctx, playbackID)
}

// Hangup ends the call and triggers finalize as a fallback. The relay's observe-close
// path is the normal trigger; this covers a caller that stops draining Inbound.
func (t *teeSession) Hangup(ctx context.Context) error {
	err := t.inner.Hangup(ctx)
	t.triggerFinalize()
	return err
}

// relay forwards inbound audio unchanged and captures a copy. It owns closing t.out.
func (t *teeSession) relay() {
	defer close(t.out)
	for {
		select {
		case chunk, ok := <-t.inner.Inbound():
			if !ok {
				t.triggerFinalize()
				return
			}
			if len(chunk.Data) > 0 {
				t.inMu.Lock()
				t.inPCM, t.inCap = appendCapped(t.inPCM, chunk.Data, t.inCap, t)
				t.inMu.Unlock()
			}
			select {
			case t.out <- chunk:
			case <-t.done:
				return
			}
		case <-t.done:
			return
		}
	}
}

// appendCapped appends data unless the per-direction cap is reached. Reaching the cap is
// logged once; capture then silently stops for that direction so a pathological call
// cannot exhaust memory.
func appendCapped(dst, data []byte, warned bool, t *teeSession) ([]byte, bool) {
	if len(dst) >= maxCaptureBytes {
		if !warned {
			t.rec.log.Warn("recorder: capture cap reached; dropping further audio",
				"call_id", t.inner.ID(), "order_id", t.order.ID, "cap_bytes", maxCaptureBytes)
		}
		return dst, true
	}
	if room := maxCaptureBytes - len(dst); len(data) > room {
		data = data[:room]
	}
	return append(dst, data...), warned
}

// triggerFinalize starts finalize exactly once, off the calling goroutine so a call end
// never blocks on transcription or blob I/O.
func (t *teeSession) triggerFinalize() {
	t.finalizing.Do(func() { go t.finalize() })
}

// finalize persists the enabled artifacts. It is best-effort: every error is logged and
// never fails the call or the runner's terminal persist.
func (t *teeSession) finalize() {
	defer close(t.finalized)
	defer close(t.done)

	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()

	inPCM := t.snapshotInbound()
	if t.decision.WAV {
		t.put(ctx, domain.BlobKindRecording, string(t.inner.ID())+inboundSuffix, wavBytes(inPCM))
		outPCM := t.snapshotOutbound()
		t.put(ctx, domain.BlobKindRecording, string(t.inner.ID())+outboundSuffix, wavBytes(outPCM))
	}

	if !t.decision.Transcript {
		return
	}
	transcript, err := t.transcribe(ctx, inPCM)
	if err != nil {
		t.rec.log.Warn("recorder: transcribe failed",
			"call_id", t.inner.ID(), "order_id", t.order.ID, "error", err)
		return
	}
	text := t.transcriptText(transcript)
	t.put(ctx, domain.BlobKindTranscript, string(t.inner.ID())+transcriptSuffix, []byte(text))
}

func (t *teeSession) transcribe(ctx context.Context, inPCM []byte) (domain.Transcript, error) {
	if t.rec.stt == nil {
		return domain.Transcript{}, errNoSTT
	}
	return t.rec.stt.TranscribeFile(ctx, bytes.NewReader(inPCM), ports.BatchOptions{
		Language:    "de",
		Timestamps:  false,
		Diarization: false,
	})
}

// transcriptText is the fixed, non-secret transcript header documented in docs/recording.md.
// outcome is empty when the order had no outcome at wrap time; the terminal outcome is
// persisted by the runner after the call, which is a later, separate write.
func (t *teeSession) transcriptText(tx domain.Transcript) string {
	outcome := ""
	if t.order.Outcome != nil {
		outcome = string(t.order.Outcome.Kind)
	}
	return "insteadly transcript\n" +
		"order=" + string(t.order.ID) + "\n" +
		"call=" + string(t.inner.ID()) + "\n" +
		"clinic=" + t.order.Clinic.Name + "\n" +
		"outcome=" + outcome + "\n" +
		"---\n" +
		tx.Text
}

func (t *teeSession) snapshotInbound() []byte {
	t.inMu.Lock()
	defer t.inMu.Unlock()
	return append([]byte(nil), t.inPCM...)
}

func (t *teeSession) snapshotOutbound() []byte {
	t.outMu.Lock()
	defer t.outMu.Unlock()
	return append([]byte(nil), t.outPCM...)
}

// put writes one blob and logs (never returns) a failure.
func (t *teeSession) put(ctx context.Context, kind, name string, data []byte) {
	if t.rec.blobs == nil {
		t.rec.log.Warn("recorder: blob store is nil; skipping artifact",
			"call_id", t.inner.ID(), "order_id", t.order.ID, "kind", kind)
		return
	}
	key := domain.BlobKey{OrderID: t.order.ID, Kind: kind, Name: name}
	if err := t.rec.blobs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.rec.log.Warn("recorder: persist artifact failed",
			"call_id", t.inner.ID(), "order_id", t.order.ID, "kind", kind, "bytes", len(data), "error", err)
	}
}
