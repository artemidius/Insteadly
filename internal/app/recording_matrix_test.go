package app

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	coreagent "github.com/artemidius/insteadly/internal/core/agent"
	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
	"github.com/artemidius/insteadly/internal/core/recorder"
	"github.com/artemidius/insteadly/internal/core/runner"
)

// matrixSession is a minimal CallSession: it has already answered, delivers two inbound
// canonical frames, and ends when the runner hangs up. It makes the recording boundary
// deterministic without a network.
type matrixSession struct {
	id      domain.CallID
	events  chan domain.CallEvent
	inbound chan domain.AudioChunk

	mu       sync.Mutex
	sent     [][]byte
	hangOnce sync.Once
}

func newMatrixSession() *matrixSession {
	s := &matrixSession{
		id:      domain.NewCallID(),
		events:  make(chan domain.CallEvent, 8),
		inbound: make(chan domain.AudioChunk, 8),
	}
	s.events <- domain.CallEvent{CallID: s.id, Kind: domain.EventDialing, At: time.Now()}
	s.events <- domain.CallEvent{CallID: s.id, Kind: domain.EventRinging, At: time.Now()}
	s.events <- domain.CallEvent{CallID: s.id, Kind: domain.EventAnswered, At: time.Now()}
	s.inbound <- domain.AudioChunk{Data: matrixFrame(1)}
	s.inbound <- domain.AudioChunk{Data: matrixFrame(2)}
	close(s.inbound)
	return s
}

func matrixFrame(seed byte) []byte {
	frame := make([]byte, domain.FrameBytes)
	for i := range frame {
		frame[i] = seed
	}
	return frame
}

func (s *matrixSession) ID() domain.CallID               { return s.id }
func (s *matrixSession) Events() <-chan domain.CallEvent { return s.events }
func (s *matrixSession) Inbound() <-chan domain.AudioChunk {
	return s.inbound
}

func (s *matrixSession) SendAudio(_ context.Context, chunk domain.AudioChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, append([]byte(nil), chunk.Data...))
	return nil
}

func (s *matrixSession) SendDTMF(context.Context, string) error { return nil }

func (s *matrixSession) ClearAudio(context.Context) error { return nil }

func (s *matrixSession) MarkPlayback(context.Context, string) error { return nil }

func (s *matrixSession) Hangup(context.Context) error {
	s.hangOnce.Do(func() {
		select {
		case s.events <- domain.CallEvent{CallID: s.id, Kind: domain.EventEnded, EndReason: domain.EndAborted, At: time.Now()}:
		default:
		}
		close(s.events)
	})
	return nil
}

type matrixTelephony struct{ session *matrixSession }

func (m *matrixTelephony) Dial(context.Context, domain.Order) (ports.CallSession, error) {
	return m.session, nil
}

type matrixAgent struct{ outcome domain.Outcome }

func (m *matrixAgent) Run(context.Context, domain.Order, coreagent.AudioSession, <-chan domain.CallEvent, <-chan domain.CallEvent) (domain.Outcome, error) {
	return m.outcome, nil
}

// countingBlobs records every artifact the recorder persists.
type countingBlobs struct {
	ports.BlobStore

	mu   sync.Mutex
	keys []domain.BlobKey
}

func (c *countingBlobs) Put(ctx context.Context, key domain.BlobKey, r io.Reader) error {
	c.mu.Lock()
	c.keys = append(c.keys, key)
	c.mu.Unlock()
	return c.BlobStore.Put(ctx, key, r)
}

func (c *countingBlobs) snapshot() []domain.BlobKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]domain.BlobKey(nil), c.keys...)
}

func boolPtr(v bool) *bool { return &v }

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func matrixBookedOutcome() domain.Outcome {
	return domain.Outcome{
		Kind:       domain.OutcomeBooked,
		Detail:     "ok",
		BookedSlot: &domain.BookedSlot{DateTime: time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC)},
	}
}

// TestRecordingFlagMatrix proves the recording flags end-to-end through the real runner.
// Each case asserts the exact set of blobs written by recorder + local storage.
func TestRecordingFlagMatrix(t *testing.T) {
	cases := []struct {
		name      string
		global    recorder.Policy
		overrides *domain.RecordingFlags
		want      []string // "recording:<name>" | "transcript:<name>"
	}{
		{
			name:   "all flags off writes nothing",
			global: recorder.Policy{},
			want:   nil,
		},
		{
			name:   "recording off with no artifact selected captures nothing",
			global: recorder.Policy{Recording: true},
			want:   nil,
		},
		{
			name:   "WAV only",
			global: recorder.Policy{Recording: true, Audio: true},
			want:   []string{"recording:-inbound.wav", "recording:-outbound.wav"},
		},
		{
			name:   "transcript only",
			global: recorder.Policy{Recording: true, Transcript: true},
			want:   []string{"transcript:-transcript.txt"},
		},
		{
			name:   "WAV and transcript",
			global: recorder.Policy{Recording: true, Audio: true, Transcript: true},
			want:   []string{"recording:-inbound.wav", "recording:-outbound.wav", "transcript:-transcript.txt"},
		},
		{
			name:      "per-order override turns recording on over a global off",
			global:    recorder.Policy{},
			overrides: &domain.RecordingFlags{Recording: boolPtr(true), Audio: boolPtr(true)},
			want:      []string{"recording:-inbound.wav", "recording:-outbound.wav"},
		},
		{
			name:      "field-level audio off leaves transcript only",
			global:    recorder.Policy{Recording: true, Audio: true, Transcript: true},
			overrides: &domain.RecordingFlags{Audio: boolPtr(false)},
			want:      []string{"transcript:-transcript.txt"},
		},
		{
			name:      "per-order master gate off wins over global on",
			global:    recorder.Policy{Recording: true, Audio: true, Transcript: true},
			overrides: &domain.RecordingFlags{Recording: boolPtr(false)},
			want:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := local.New(t.TempDir())
			if err != nil {
				t.Fatalf("local.New: %v", err)
			}
			order := swapTestOrder()
			order.Recording = tc.overrides
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

			session := newMatrixSession()
			blobs := &countingBlobs{BlobStore: store.Blobs()}
			rec := recorder.New(&swapSTT{batchText: "matrix transcript"}, blobs, tc.global, testDiscardLogger())
			run, err := runner.New(runner.Config{
				Telephony:      &matrixTelephony{session: session},
				Orders:         store.Orders(),
				Agent:          &matrixAgent{outcome: matrixBookedOutcome()},
				Logger:         testDiscardLogger(),
				Recorder:       rec,
				CleanupTimeout: 3 * time.Second,
			})
			if err != nil {
				t.Fatalf("runner.New: %v", err)
			}
			final, err := run.Run(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Status != domain.OrderFinished {
				t.Fatalf("status = %q, want finished", final.Status)
			}

			if len(tc.want) == 0 {
				// With Capture=false, Wrap returns the unchanged session: there is no
				// finalize goroutine at all, so zero Put calls is already settled.
				time.Sleep(50 * time.Millisecond)
				if got := blobs.snapshot(); len(got) != 0 {
					t.Fatalf("unexpected blobs: %+v", got)
				}
				return
			}
			waitSwap(t, "recording artifacts", func() bool { return len(blobs.snapshot()) >= len(tc.want) })
			// Give a would-be extra artifact a moment to appear, then assert exactness.
			time.Sleep(100 * time.Millisecond)

			got := make([]string, 0, len(blobs.snapshot()))
			for _, k := range blobs.snapshot() {
				name := strings.TrimPrefix(k.Name, string(session.id))
				got = append(got, k.Kind+":"+name)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("blobs = %v, want %v", got, want)
			}

			// The artifacts carry real data, not placeholders.
			for _, k := range blobs.snapshot() {
				if k.OrderID != created.ID {
					t.Errorf("blob %+v is not tied to the order", k)
				}
			}
			if containsPrefix(got, "recording:") {
				data := readMatrixBlob(t, store, domain.BlobKey{
					OrderID: created.ID, Kind: domain.BlobKindRecording, Name: string(session.id) + "-inbound.wav",
				})
				if len(data) != 44+2*domain.FrameBytes {
					t.Errorf("inbound WAV size = %d, want header + two frames", len(data))
				}
				if got := binary.LittleEndian.Uint32(data[40:44]); got != 2*domain.FrameBytes {
					t.Errorf("data chunk = %d", got)
				}
			}
			if containsPrefix(got, "transcript:") {
				data := readMatrixBlob(t, store, domain.BlobKey{
					OrderID: created.ID, Kind: domain.BlobKindTranscript, Name: string(session.id) + "-transcript.txt",
				})
				if !strings.Contains(string(data), "matrix transcript") {
					t.Errorf("transcript = %q", data)
				}
			}
		})
	}
}

func containsPrefix(items []string, prefix string) bool {
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func readMatrixBlob(t *testing.T, store *local.Store, key domain.BlobKey) []byte {
	t.Helper()
	rc, err := store.Blobs().Open(context.Background(), key)
	if err != nil {
		t.Fatalf("open blob %+v: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	return data
}
