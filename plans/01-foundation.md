# Chunk C0 — Foundation: hexagon scaffold (plan for the coding agent)

> **Status:** planned, not started. Chunk owner: next coding session.
> **Source of truth:** [DESIGN.md](../DESIGN.md) — this plan derives from it and must not
> contradict it. If a conflict appears, stop and resolve it against DESIGN.md, not silently.
> **Progress register:** [PROGRESS.md](../PROGRESS.md) — update it at the end of the session
> that implements this chunk (see its "Update protocol").

---

## 1. What this chunk is

The first implementation slice of **M0 — skeleton walk** (DESIGN.md §5). C0 builds the entire
hexagon *skeleton* and the first real adapter (local storage), with **deterministic stub
adapters** standing in for Twilio and the Mistral chain. Everything compiles, everything is
unit/integration tested, the binary boots and serves a working HTTP API — **with zero external
network calls and zero credentials required**.

This chunk proves the dependency-inversion promise early (DESIGN.md §1.2): the core depends
only on port interfaces, adapters sit behind those ports, and only the composition root knows
concrete providers. Later chunks merely swap stubs for real adapters.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | Go module + repo scaffolding (`.gitignore`, `Makefile`) | repo root |
| 2 | Domain model: order state machine, outcomes, call events, canonical audio, blob keys | `internal/core/domain/` |
| 3 | All six port contracts as Go interfaces (Telephony, STT, TTS, Dialogue, Storage, Payments, Notifier) | `internal/core/ports/` |
| 4 | Config loader: env-based, `INSTEADLY_` prefix, `.env` support, strict validation, secret-safe summaries | `internal/core/config/` |
| 5 | Local storage adapter (orders as JSON files + blob files) | `internal/adapters/storage/local/` |
| 6 | Stub adapters for Telephony, STT, TTS, Dialogue (temporary providers, permanent test doubles) | `internal/adapters/stub/` |
| 7 | Composition root (the only place importing core + adapters) | `internal/app/` |
| 8 | HTTP API: health, order create/list/get, JSON envelopes, logging middleware, graceful shutdown | `internal/httpapi/`, `cmd/insteadly/` |
| 9 | Tests for everything above + hexagon guard (`make check-deps`) | `*_test.go` |

### Out of scope (do NOT implement — later chunks)

- **Anything Twilio** (REST, webhooks, Media Streams WebSocket, µ-law conversion) → C1.
- **Anything Mistral** (STT/TTS/Dialogue WebSocket or HTTP clients) → C2.
- **Agent engine** (conversation loop, VAD, turn-taking, barge-in, DTMF handling, outcome
  extraction) → C3/C4. C0 only defines the ports it will consume.
- **Scheduler**, retry/backoff policy, opening windows → C5.
- **Payments** (Stripe) → C6. The port is *declared* now, no adapter.
- **Notifications** → C7. Port declared now, no adapter.
- **Postgres / River**, Docker, tunnels (cloudflared/ngrok) → C5 / C1 / C3 respectively.
- **Web app (React)**, SSE, compliance/GDPR → later milestones.

If you feel any of the above creeping into C0, stop — that is scope creep. Note it in the
session log of PROGRESS.md instead.

---

## 2. Preconditions (verify first, in this order)

1. **Install Go** — *not installed on this machine as of 2026-11-16* (`go: command not found`,
   checked in bash, zsh-login, `/opt/homebrew`, `/usr/local/go`).
   ```bash
   brew install go
   go version   # record the exact version in PROGRESS.md
   ```
   Any Go ≥ 1.22 works (required for `http.ServeMux` method patterns). Latest stable is fine.
2. `git -C /Users/q/Insteadly status` → no commits yet; you are expected **not to commit**
   unless the user asks. The repo has **no remote** — do not invent one.
3. `.env` currently contains (names only — never print values):
   `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `TWILIO_PHONE_NUMBER`, `TEST_PHONE`.
   **C0 must not read any of them.** Missing `MISTRAL_API_KEY` is irrelevant until C2.
4. Confirm DESIGN.md §1–§2 still describes the intended system; if the user amended it since
   this plan was written, re-read it and check for conflicts.

---

## 3. Module and layout

Module path: **`github.com/artemidius/insteadly`** (the project is called "Insteadly").

```bash
cd /Users/q/Insteadly
go mod init github.com/artemidius/insteadly
```

(No remote exists yet; this path is a placeholder that can be renamed later in one `go.mod`
edit plus import rewrite. It is chosen because `gh` is authenticated as `artemidius`.) If the
user gives you a real remote in-session, use its path instead and do a global import rewrite.

Expected tree after C0 (new files only; `+` = create):

```
Insteadly/
├── DESIGN.md                      (existing, read-only)
├── PROGRESS.md                    (existing, update at session end)
├── plans/01-foundation.md         (this file, read-only during implementation)
├── go.mod                         +
├── .gitignore                     +
├── Makefile                       +
├── cmd/insteadly/
│   └── main.go                    +
└── internal/
    ├── core/                      # THE HEXAGON — imports nothing provider-specific
    │   ├── domain/
    │   │   ├── ids.go             +
    │   │   ├── order.go           +   (+ order_test.go)
    │   │   ├── call.go            +
    │   │   ├── audio.go           +
    │   │   ├── tools.go           +
    │   │   └── notify.go          +
    │   ├── ports/                 # contracts consumed by the core
    │   │   ├── errors.go          +
    │   │   ├── telephony.go       +
    │   │   ├── speech.go          +   (SpeechToText + TextToSpeech)
    │   │   ├── dialogue.go        +
    │   │   ├── storage.go         +
    │   │   ├── payments.go        +
    │   │   └── notify.go          +
    │   └── config/
    │       ├── config.go          +   (+ config_test.go)
    │       └── env.go             +   (loader; covered by config_test.go)
    ├── adapters/
    │   ├── storage/local/
    │   │   ├── local.go           +   (+ local_test.go)
    │   │   ├── orders.go          +
    │   │   └── blobs.go           +
    │   └── stub/                  # fake providers — permanent test doubles
    │       ├── telephony.go       +
    │       ├── stt.go             +
    │       ├── tts.go             +
    │       └── dialogue.go        +   (+ stub_test.go)
    ├── app/
    │   └── app.go                 +   (composition root)
    └── httpapi/
        ├── server.go              +   (mux, middleware, healthz)
        ├── orders.go              +   (handlers)
        └── server_test.go         +
```

The project name is "Insteadly" (nine letters, i-n-s-t-e-a-d-l-y). The module path, the
`cmd/insteadly` directory, and the binary name all use exactly that spelling. Double-check
every import you write against this rule.

Import-direction law (enforced by `make check-deps`):

```
cmd/insteadly → internal/app, internal/httpapi, internal/core/config
internal/app  → internal/core/{config,ports,domain}, internal/adapters/*   ← composition root only
internal/httpapi → internal/core/{ports,domain,config}, internal/app
internal/adapters/* → internal/core/{ports,domain}
internal/core/ports → internal/core/domain
internal/core/domain → stdlib only
```

`internal/core` must never import `internal/adapters` or any external provider SDK. Nothing
outside `internal/core` may implement core logic. No package may import `cmd/`.

---

## 4. Implementation steps (do them in this order)

### Step 1 — Scaffolding

1. Create the module:
   ```bash
   go mod init github.com/artemidius/insteadly
   ```
2. Create `.gitignore`:
   ```
   /bin/
   /data/
   /.env
   .idea/
   *.log
   ```
   (`/data/` is runtime storage; `/.env` holds secrets and must never be committed.)
3. Create `Makefile` (tabs, not spaces):

   ```make
   BIN      := bin/insteadly
   VERSION  ?= dev

   .PHONY: build run test lint fmt check-deps clean clean-data

   build:
   	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/insteadly

   run:
   	go run ./cmd/insteadly

   test:
   	go test -race -count=1 ./...

   lint:
   	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
   	go vet ./...
   	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
   	else echo "golangci-lint not installed — skipped (optional: brew install golangci-lint)"; fi

   check-deps:
   	@if go list -deps ./internal/core/... | grep -q 'insteadly/internal/adapters'; then \
   		echo "FAIL: internal/core imports an adapter — hexagon violated"; exit 1; \
   	else echo "OK: core imports no adapters"; fi

   fmt:
   	gofmt -w .

   clean:
   	rm -rf bin

   clean-data:
   	rm -rf data
   ```

   (`lint` must pass even without golangci-lint installed — it is optional on this machine.)
4. Verify: `make lint` → "golangci-lint not installed — skipped", no errors. `make build`
   fails (no packages yet) — expected, continue.

### Step 2 — `internal/core/domain` (no dependencies)

Pure types + state machine. No I/O, no `time.Now()` except in `Transition`/`Finish`
(timestamping; clock injection is deliberately deferred to C5).

**`ids.go`** — random hex IDs:

```go
package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// OrderID identifies an order; CallID identifies one call attempt.
type OrderID string
type CallID string

func NewOrderID() OrderID { return OrderID(newID()) }
func NewCallID() CallID   { return CallID(newID()) }

// newID returns 16 random bytes as lowercase hex (32 chars).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
```

**`order.go`** — the spine state machine (DESIGN.md §2.1.1). Use exactly these statuses,
outcome kinds, and this transition table:

```go
package domain

import (
	"errors"
	"fmt"
	"time"
)

type OrderStatus string

const (
	OrderCreated   OrderStatus = "created"
	OrderPaid      OrderStatus = "paid"
	OrderScheduled OrderStatus = "scheduled"
	OrderDialing   OrderStatus = "dialing"
	OrderRinging   OrderStatus = "ringing"
	OrderInCall    OrderStatus = "in_call"
	OrderFinished  OrderStatus = "finished"
)

type OutcomeKind string

const (
	OutcomeBooked         OutcomeKind = "booked"
	OutcomeCallbackNeeded OutcomeKind = "callback_needed"
	OutcomeFailed         OutcomeKind = "failed"
	OutcomeAborted        OutcomeKind = "aborted"
)

// Order is the spine every subsystem hangs off (DESIGN.md §2.1.1).
type Order struct {
	ID        OrderID         `json:"id"`
	Status    OrderStatus     `json:"status"`
	Clinic    Clinic          `json:"clinic"`
	Patient   Patient         `json:"patient"`
	Outcome   *Outcome        `json:"outcome"`
	Recording *RecordingFlags `json:"recording"` // per-order overrides; nil = global defaults
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Attempts  int             `json:"attempts"`
}

type Clinic struct {
	Name  string `json:"name"`
	Phone string `json:"phone"` // E.164-ish; free text for now, validated against a directory in C5
}

type Patient struct {
	FullName        string      `json:"full_name"`
	Phone           string      `json:"phone"`
	BirthDate       string      `json:"birth_date"` // YYYY-MM-DD, optional for now
	InsuranceNumber string      `json:"insurance_number"`
	Reason          string      `json:"reason"`
	PreferredSlots  []TimeRange `json:"preferred_slots"`
}

type TimeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RecordingFlags are per-order policy overrides (DESIGN.md §3.4). nil pointer = use global.
type RecordingFlags struct {
	Recording  *bool `json:"recording"`
	Transcript *bool `json:"transcript"`
	Audio      *bool `json:"audio"`
}

type Outcome struct {
	Kind       OutcomeKind `json:"kind"`
	Detail     string      `json:"detail"`
	BookedSlot *BookedSlot `json:"booked_slot,omitempty"`
	CallbackAt *time.Time  `json:"callback_at,omitempty"`
	FailReason string      `json:"fail_reason,omitempty"`
}

type BookedSlot struct {
	DateTime time.Time `json:"date_time"`
	Doctor   string    `json:"doctor"`
	Practice string    `json:"practice"`
}

var (
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrInvalidOutcome    = errors.New("invalid outcome")
)

// Transition table. "finished" is NOT reachable via Transition — only via Finish
// (which enforces a valid Outcome). Retry-from-failed is a C5 scheduler concern and
// must NOT be added here.
var transitions = map[OrderStatus]map[OrderStatus]struct{}{
	OrderCreated:   {OrderPaid: {}},
	OrderPaid:      {OrderScheduled: {}},
	OrderScheduled: {OrderDialing: {}},
	OrderDialing:   {OrderRinging: {}},
	OrderRinging:   {OrderInCall: {}},
	OrderInCall:    {},
}

func (o *Order) Transition(next OrderStatus) error {
	targets, ok := transitions[o.Status]
	if !ok {
		return fmt.Errorf("%w: from %q", ErrInvalidTransition, o.Status)
	}
	if _, ok := targets[next]; !ok {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidTransition, o.Status, next)
	}
	o.Status = next
	o.UpdatedAt = time.Now()
	return nil
}

func (o *Order) Finish(out Outcome) error {
	if o.Status == OrderFinished {
		return fmt.Errorf("%w: already finished", ErrInvalidTransition)
	}
	if err := validateOutcome(out); err != nil {
		return err
	}
	o.Outcome = &out
	o.Status = OrderFinished
	o.UpdatedAt = time.Now()
	return nil
}

func (o Order) Terminal() bool { return o.Status == OrderFinished }

func validateOutcome(out Outcome) error {
	switch out.Kind {
	case OutcomeBooked:
		if out.BookedSlot == nil || out.BookedSlot.DateTime.IsZero() {
			return fmt.Errorf("%w: booked requires booked_slot", ErrInvalidOutcome)
		}
	case OutcomeCallbackNeeded:
		if out.CallbackAt == nil {
			return fmt.Errorf("%w: callback_needed requires callback_at", ErrInvalidOutcome)
		}
	case OutcomeFailed:
		if out.FailReason == "" {
			return fmt.Errorf("%w: failed requires fail_reason", ErrInvalidOutcome)
		}
	case OutcomeAborted:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidOutcome, out.Kind)
	}
	return nil
}
```

**`call.go`** — call events, end reasons, transcript types (DESIGN.md §2.2 Telephony/STT rows):

```go
package domain

import "time"

type CallEventKind string

const (
	EventDialing  CallEventKind = "dialing"
	EventRinging  CallEventKind = "ringing"
	EventAnswered CallEventKind = "answered"
	EventDTMF     CallEventKind = "dtmf"
	EventEnded    CallEventKind = "ended"
	EventError    CallEventKind = "error"
)

// EndReason normalizes provider hangup causes so the core never sees provider codes.
type EndReason string

const (
	EndCompleted  EndReason = "completed"    // normal end after a result
	EndBusy       EndReason = "busy"
	EndNoAnswer   EndReason = "no_answer"
	EndRejected   EndReason = "rejected"
	EndIVRDeadEnd EndReason = "ivr_dead_end" // walked an IVR that went nowhere
	EndAborted    EndReason = "aborted"      // user/system cancelled
	EndError      EndReason = "error"
)

type CallEvent struct {
	CallID    CallID        `json:"call_id"`
	Kind      CallEventKind `json:"kind"`
	DTMF      string        `json:"dtmf,omitempty"`
	EndReason EndReason     `json:"end_reason,omitempty"`
	At        time.Time     `json:"at"`
}

// Transcript is the product of batch (post-call) transcription.
type Transcript struct {
	Text     string
	Segments []TranscriptSegment // empty unless timestamps/diarization requested
}

type TranscriptSegment struct {
	Start   time.Duration
	End     time.Duration
	Text    string
	Speaker string
}
```

**`audio.go`** — canonical audio contract (DESIGN.md §1.5):

```go
package domain

// Canonical audio everywhere inside the core: PCM16 little-endian, mono, 16 kHz.
// All codec/sample-rate conversion happens inside adapters (e.g. Twilio's 8 kHz µ-law
// is converted by the Twilio adapter). Providers must never leak formats past the
// port boundary.
const (
	SampleRate = 16000
	Channels   = 1
	BitDepth   = 16
	FrameMs    = 20
	FrameBytes = SampleRate * Channels * BitDepth / 8 * FrameMs / 1000 // = 640
)

// AudioChunk is a slice of canonical PCM16 LE mono 16 kHz audio.
type AudioChunk struct {
	Data []byte
}

// Blob keys for the storage port.
const (
	BlobKindRecording  = "recording"
	BlobKindTranscript = "transcript"
)

type BlobKey struct {
	OrderID OrderID // "" = system-wide blob (not tied to an order)
	Kind    string  // BlobKind*; storage validates against the allowlist
	Name    string  // filename, e.g. "2026-11-20T09-15-00Z.wav"
}
```

**`tools.go`** — the tool contract for the Dialogue port (DESIGN.md §6 Q1; the exact tool set
is resolved in C3, the type is needed now):

```go
package domain

// Tool is a tool the Dialogue model may invoke (propose_slot, confirm_booking,
// capture_insurance, escalate_to_human, ...).
type Tool struct {
	Name        string
	Description string
	Parameters  []byte // JSON Schema for the arguments
}
```

**`notify.go`** — notification + payment event shapes (ports are declared in C0, adapters in C6/C7):

```go
package domain

import "time"

type Notification struct {
	OrderID OrderID
	Channel string // "email" | "sms"
	To      string
	Subject string
	Body    string
}

type PaymentEvent struct {
	OrderID  OrderID
	Paid     bool
	Refunded bool
	At       time.Time
}
```

**`order_test.go`** — table-driven tests (see §6 for the full test list).

Verify: `go test ./internal/core/domain/`.

### Step 3 — `internal/core/ports` (contracts only, no implementations)

One file per port. Every method needs a doc comment explaining **error semantics, channel
semantics, and backpressure rules** — the doc comment IS the contract prose from DESIGN.md
§2.2, made executable. Do not add methods you don't need yet; do not remove any listed here.

**`errors.go`**:

```go
package ports

import "errors"

// Sentinel errors the core expects from adapters.
var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrNotImplemented = errors.New("not implemented")
)
```

**`telephony.go`**:

```go
package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Telephony dials numbers and exposes per-call event + audio streams.
// The core never sees provider concepts (CallSid, streams, µ-law): the adapter
// normalizes everything to domain.CallEvent and canonical audio.
type Telephony interface {
	// Dial initiates an outbound call to order.Clinic.Phone. It returns a live
	// CallSession without waiting for the far end to answer; progress arrives
	// as events. Errors are terminal for this attempt (never partial sessions).
	Dial(ctx context.Context, order domain.Order) (CallSession, error)
}

// CallSession is one live call attempt.
type CallSession interface {
	ID() domain.CallID

	// Events delivers normalized call progress. The channel is closed when the
	// call ends; the final event is always EventEnded (or EventError followed by
	// EventEnded). Consumers must drain until close.
	Events() <-chan domain.CallEvent

	// Inbound delivers far-end audio in canonical format (PCM16 LE mono 16 kHz).
	// The adapter buffers boundedly (e.g. 100 chunks) and DROPS-OLDEST when the
	// core is slow, counting drops in a log warning — the core must keep up.
	// Closed when the call ends.
	Inbound() <-chan domain.AudioChunk

	// SendAudio enqueues canonical audio for playback in the call. Must be safe
	// for concurrent use and must return (not panic) after the call has ended.
	SendAudio(ctx context.Context, chunk domain.AudioChunk) error

	// SendDTMF sends a digit sequence through the live call (IVR menus).
	SendDTMF(ctx context.Context, digits string) error

	// Hangup terminates the call. Idempotent; safe after natural end.
	Hangup(ctx context.Context) error
}
```

**`speech.go`** (both STT and TTS — same file, they are both "speech" ports):

```go
package ports

import (
	"context"
	"io"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// SpeechToText is pure streaming transcription (DESIGN.md §1.6: turn-taking/VAD live
// in the core, not here). Adapters must emit events for canonical audio only.
type SpeechToText interface {
	// TranscribeStream opens a live streaming transcription session.
	TranscribeStream(ctx context.Context, opts StreamOptions) (TranscriptionStream, error)

	// TranscribeFile transcribes a complete recording (post-call). The reader
	// yields canonical audio. Adapters may read it fully before returning.
	TranscribeFile(ctx context.Context, r io.Reader, opts BatchOptions) (domain.Transcript, error)
}

type StreamOptions struct {
	Language string   // BCP-47, "" = provider default
	Hints    []string // context-biasing vocabulary (clinic names, streets); may be ignored
}

type BatchOptions struct {
	Language    string
	Hints       []string
	Timestamps  bool
	Diarization bool
}

type TranscriptionStream interface {
	// Feed canonical audio. Safe for concurrent use; must not block indefinitely
	// if the provider is slow (bounded internal queue, drop-oldest like Telephony).
	Feed(ctx context.Context, chunk domain.AudioChunk) error

	// Results delivers transcript events (partial then final). Closed when the
	// stream ends. A final event for the last audio is guaranteed only if Close
	// is called; adapters should flush on Close.
	Results() <-chan TranscriptionEvent

	// Close ends the session and releases provider resources. Idempotent.
	Close(ctx context.Context) error
}

type TranscriptionEvent struct {
	Text  string
	Final bool
	At    time.Time
}

// TextToSpeech turns text chunks into canonical audio chunks, spoken as generated
// (DESIGN.md §2.2: LLM output is spoken while it streams).
type TextToSpeech interface {
	// Speak opens a streaming TTS session.
	Speak(ctx context.Context, opts SpeakOptions) (SpeechStream, error)
}

type SpeakOptions struct {
	Voice string // provider voice id; "" = provider default
}

type SpeechStream interface {
	// FeedText enqueues text to synthesize, in order. Audio arrives on Audio()
	// as it is produced (chunking is provider-dependent; the core must not
	// assume 1:1 text-to-audio correspondence).
	FeedText(ctx context.Context, text string) error

	// Audio delivers canonical PCM16 chunks. Closed after the last text fed
	// before Close has been synthesized.
	Audio() <-chan domain.AudioChunk

	// Close flushes and releases provider resources. Idempotent.
	Close(ctx context.Context) error
}
```

**`dialogue.go`**:

```go
package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Dialogue is the booking brain: conversation + tools -> streamed replies, tool
// calls, and the structured outcome (DESIGN.md §2.1.3). Providers receive only
// text and tool definitions — no audio, no provider knowledge leaks in.
type Dialogue interface {
	// NewSession starts a booking conversation for one order. OutcomeSchema is
	// the JSON Schema the final domain.Outcome must satisfy; the adapter may use
	// it for constrained output.
	NewSession(ctx context.Context, req SessionRequest) (DialogueSession, error)
}

type SessionRequest struct {
	OrderID       domain.OrderID
	SystemPrompt  string   // optional override; "" = built-in booking prompt
	Tools         []domain.Tool
	OutcomeSchema []byte // JSON Schema
}

type DialogueSession interface {
	// SendUserTurn feeds the next user utterance and returns one assistant turn.
	SendUserTurn(ctx context.Context, text string) (AssistantTurn, error)

	// SubmitToolResults returns tool execution results for tool calls made during
	// the previous turn; the model continues from them and produces a new turn.
	SubmitToolResults(ctx context.Context, results []ToolResult) (AssistantTurn, error)

	Close(ctx context.Context) error
}

// AssistantTurn represents one assistant reply. TextChunks streams partial text;
// Wait blocks until the turn is complete and reports tool calls + final outcome.
// Consumers MUST drain TextChunks until close before calling Wait.
type AssistantTurn interface {
	TextChunks() <-chan string
	Wait(ctx context.Context) (TurnResult, error)
}

type TurnResult struct {
	ToolCalls []ToolCall
	Outcome   *domain.Outcome // non-nil when the model finalized the booking outcome
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments []byte // JSON object
}

type ToolResult struct {
	ID     string
	Result []byte // JSON, tool-defined shape
}
```

**`storage.go`**:

```go
package ports

import (
	"context"
	"io"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Storage persists order state and blobs (DESIGN.md §2.2). Implementations may be
// local disk (C0) or Postgres+S3/MinIO (later); the core sees one interface.
type Storage interface {
	Orders() OrderStore
	Blobs() BlobStore
}

type OrderStore interface {
	// Create persists a new order. If o.ID is empty the store assigns one
	// (returned). Returns ErrConflict if the ID already exists.
	Create(ctx context.Context, o domain.Order) (domain.Order, error)

	// Get returns ErrNotFound if the order does not exist.
	Get(ctx context.Context, id domain.OrderID) (domain.Order, error)

	// List returns all orders, newest first (by CreatedAt).
	List(ctx context.Context) ([]domain.Order, error)

	// Update atomically applies fn to the stored order and persists the result.
	// fn receives a copy; it must not touch o.ID. fn runs under the store's lock,
	// so it must be fast and must not call back into the store. Returns the
	// updated order.
	Update(ctx context.Context, id domain.OrderID, fn func(*domain.Order) error) (domain.Order, error)
}

type BlobStore interface {
	// Put stores a blob under key. Safe to call with the same key repeatedly
	// (last write wins).
	Put(ctx context.Context, key domain.BlobKey, r io.Reader) error

	// Open returns a reader for the blob, or ErrNotFound.
	Open(ctx context.Context, key domain.BlobKey) (io.ReadCloser, error)

	// Delete removes a blob. Idempotent: deleting a missing blob is not an error.
	Delete(ctx context.Context, key domain.BlobKey) error
}
```

**`payments.go`** (declared, no adapter until C6):

```go
package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Payments charges and refunds orders (DESIGN.md §2.2). First adapter: Stripe, C6.
type Payments interface {
	// CreateCheckout starts a payment for the order and returns a redirect URL.
	CreateCheckout(ctx context.Context, order domain.Order) (redirectURL string, err error)

	// HandleWebhook processes a provider webhook. payload is the raw body,
	// signature the provider's signature header. Never trust payload alone.
	HandleWebhook(ctx context.Context, payload []byte, signature string) (domain.PaymentEvent, error)
}
```

**`notify.go`**:

```go
package ports

import (
	"context"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// Notifier sends confirmations and outcomes (DESIGN.md §2.2). No adapter in C0-C6;
// first adapter lands in C7.
type Notifier interface {
	Send(ctx context.Context, n domain.Notification) error
}
```

Verify: `go build ./...` (everything so far compiles; `gofmt -l .` clean).

### Step 4 — `internal/core/config`

**`config.go`** — the config surface mirrors DESIGN.md §2.3:

```go
package config

import "fmt"

type Config struct {
	HTTP      HTTP
	Telephony Telephony
	STT       STT
	TTS       TTS
	Dialogue  Dialogue
	Storage   Storage
	Recording Policy
	Mistral   Mistral // shared by STT/TTS/Dialogue when their provider == "mistral"
	Log       Log
}

type HTTP struct {
	Addr string // host:port
}

type Telephony struct {
	Provider string // "twilio" | "stub"
	Twilio   Twilio
}

type Twilio struct {
	AccountSID  string // secret
	AuthToken   string // secret
	PhoneNumber string // caller-id number bought in Twilio console (needed C1)
	Mode        string // "voice-api" | "sip-trunk" (needed C1)
}

type STT struct{ Provider string } // "mistral" | "stub"

type TTS struct{ Provider string } // "mistral" | "stub"

type Dialogue struct{ Provider string } // "mistral" | "stub"

type Storage struct {
	Provider string // "local"
	Local    Local
}

type Local struct {
	Dir string // data dir, default "./data"
}

type Policy struct { // global recording defaults (DESIGN.md §3.4)
	RecordingEnabled  bool
	TranscriptEnabled bool
	AudioEnabled      bool
}

type Mistral struct {
	APIKey string // secret; required only when a mistral provider is selected (C2)
}

type Log struct {
	Level  string // debug|info|warn|error
	Format string // text|json
}

// Validate checks enum values and required fields. It does NOT check whether an
// adapter for a provider exists yet — app.New does that (so "twilio" passes here
// and fails with ErrNotImplemented in the composition root until C1).
func (c Config) Validate() error {
	// providers: telephony ∈ {twilio, stub}; stt/tts/dialogue ∈ {mistral, stub};
	// storage ∈ {local}; log.level ∈ {debug,info,warn,error}; log.format ∈ {text,json};
	// http.addr non-empty; storage.local.dir non-empty.
	// Use fmt.Errorf("config: ...") and return the FIRST error found.
}

// Summary returns non-secret values for startup logging and /healthz.
// NEVER include AccountSID, AuthToken, APIKey.
func (c Config) Summary() map[string]any {
	return map[string]any{
		"http.addr":                  c.HTTP.Addr,
		"telephony.provider":         c.Telephony.Provider,
		"stt.provider":               c.STT.Provider,
		"tts.provider":               c.TTS.Provider,
		"dialogue.provider":          c.Dialogue.Provider,
		"storage.provider":           c.Storage.Provider,
		"storage.local.dir":          c.Storage.Local.Dir,
		"recording.enabled":          c.Recording.RecordingEnabled,
		"transcript.enabled":         c.Recording.TranscriptEnabled,
		"audio.enabled":              c.Recording.AudioEnabled,
		"log.level":                  c.Log.Level,
		"log.format":                 c.Log.Format,
	}
}
```

**`env.go`** — the loader. Contract:

- Prefix **`INSTEADLY_`**; dots in config paths become underscores.
- Precedence: **real environment > `.env` file > defaults**. `.env` is loaded only if it
  exists; its `INSTEADLY_*` lines are applied *first*, then `os.Environ()` overrides.
- `.env` parser: one `KEY=VALUE` per line; skip blank lines and `#` comments; strip
  surrounding whitespace; **no quotes/escapes support** (document this; Twilio/Mistral tokens
  are plain alphanumerics).
- **Strict mode:** any `INSTEADLY_*` variable not in the registry is a hard error at startup
  (typo protection — a mis-typed recording toggle silently disabling recording is worse than
  a crash). Non-`INSTEADLY_` vars (e.g. existing `TWILIO_ACCOUNT_SID` in `.env`) are ignored.
- Secrets never appear in errors or logs (a parse error reports the *name*, never the value).

Registry pattern (keeps strict-mode check, parsing, and docs in one place):

```go
// env.go (skeleton — fill every entry)
type setter struct {
	apply  func(*Config, string) error
	secret bool // only used for documentation; values are never logged anyway
}

var registry = map[string]setter{
	"INSTEADLY_HTTP_ADDR":            {apply: strSetter(func(c *Config, v string) { c.HTTP.Addr = v })},
	"INSTEADLY_LOG_LEVEL":            {apply: strSetter(func(c *Config, v string) { c.Log.Level = v })},
	"INSTEADLY_LOG_FORMAT":           {apply: strSetter(func(c *Config, v string) { c.Log.Format = v })},
	"INSTEADLY_TELEPHONY_PROVIDER":   {apply: strSetter(func(c *Config, v string) { c.Telephony.Provider = v })},
	"INSTEADLY_TWILIO_ACCOUNT_SID":   {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.AccountSID = v }), secret: true},
	"INSTEADLY_TWILIO_AUTH_TOKEN":    {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.AuthToken = v }), secret: true},
	"INSTEADLY_TWILIO_PHONE_NUMBER":  {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.PhoneNumber = v })},
	"INSTEADLY_TWILIO_MODE":          {apply: strSetter(func(c *Config, v string) { c.Telephony.Twilio.Mode = v })},
	"INSTEADLY_STT_PROVIDER":         {apply: strSetter(func(c *Config, v string) { c.STT.Provider = v })},
	"INSTEADLY_TTS_PROVIDER":         {apply: strSetter(func(c *Config, v string) { c.TTS.Provider = v })},
	"INSTEADLY_DIALOGUE_PROVIDER":    {apply: strSetter(func(c *Config, v string) { c.Dialogue.Provider = v })},
	"INSTEADLY_STORAGE_PROVIDER":     {apply: strSetter(func(c *Config, v string) { c.Storage.Provider = v })},
	"INSTEADLY_STORAGE_LOCAL_DIR":    {apply: strSetter(func(c *Config, v string) { c.Storage.Local.Dir = v })},
	"INSTEADLY_RECORDING_ENABLED":    {apply: boolSetter(func(c *Config, v bool) { c.Recording.RecordingEnabled = v })},
	"INSTEADLY_TRANSCRIPT_ENABLED":   {apply: boolSetter(func(c *Config, v bool) { c.Recording.TranscriptEnabled = v })},
	"INSTEADLY_AUDIO_ENABLED":        {apply: boolSetter(func(c *Config, v bool) { c.Recording.AudioEnabled = v })},
	"INSTEADLY_MISTRAL_API_KEY":      {apply: strSetter(func(c *Config, v string) { c.Mistral.APIKey = v }), secret: true},
}

func strSetter(fn func(*Config, string)) func(*Config, string) error {
	return func(c *Config, v string) error { fn(c, v); return nil }
}

func boolSetter(fn func(*Config, bool)) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("expected true/false, got %q", v)
		}
		fn(c, b)
		return nil
	}
}

func defaults() Config {
	return Config{
		HTTP:    HTTP{Addr: "127.0.0.1:8080"},
		Telephony: Telephony{Provider: "stub", Twilio: Twilio{Mode: "voice-api"}},
		STT:      STT{Provider: "stub"},
		TTS:      TTS{Provider: "stub"},
		Dialogue: Dialogue{Provider: "stub"},
		Storage:  Storage{Provider: "local", Local: Local{Dir: "./data"}},
		Log:      Log{Level: "info", Format: "text"},
	}
}

// Load applies defaults, then ./.env (if present), then the process environment,
// then validates. Strict about unknown INSTEADLY_* names.
func Load() (Config, error) {
	c := defaults()
	if err := applyDotEnv(&c); err != nil {
		return c, err
	}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "INSTEADLY_") {
			continue
		}
		s, ok := registry[name]
		if !ok {
			return c, fmt.Errorf("config: unknown variable %q", name)
		}
		if err := s.apply(&c, val); err != nil {
			return c, fmt.Errorf("config: %s: %w", name, err)
		}
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
```

Default providers are **stub** so a fresh checkout boots with zero configuration — that is
the C0 demo property. Do not default to `twilio`/`mistral`.

Note for C1: the existing `.env` holds `TWILIO_ACCOUNT_SID`/`TWILIO_AUTH_TOKEN` (unprefixed).
C0 ignores them; C1's plan will decide whether to add fallback reading. Do not add it now.

Verify: `go build ./...`; write `config_test.go` (see §6) and `go test ./internal/core/config/`.

### Step 5 — `internal/adapters/storage/local`

Package `local` implements `ports.Storage` (and `ports.OrderStore`/`ports.BlobStore` —
`Orders()`/`Blobs()` return the store itself).

Layout under `Dir`:

```
data/
├── orders/<32-hex-id>.json
└── blobs/<32-hex-id>/<kind>/<name>
```

**Requirements:**

1. `New(dir string) (*Store, error)` — creates `orders/` and `blobs/` eagerly (fail fast on
   unwritable dir). Struct: `type Store struct { mu sync.RWMutex; dir string }`.
2. **Path safety (mandatory):** every `OrderID` embedded in a path must match
   `^[0-9a-f]{32}$`, and every `BlobKey.Kind` must be in `{recording, transcript}`, and
   `BlobKey.Name` must be non-empty, contain no `/`, and not be `.` or `..`. Reject with
   `fmt.Errorf("invalid ...")` otherwise. Never join user input into paths without these
   checks (classic traversal bug).
3. `Create`: if `o.ID == ""` assign `domain.NewOrderID()`; if the file exists → wrap
   `ports.ErrConflict`; set `CreatedAt`/`UpdatedAt` to now if zero; `Status` forced to
   `created` (storage does not run the state machine, but a new record must start at
   `created`); persist; return the stored order.
4. `Get`: missing file → `fmt.Errorf("order %s: %w", id, ports.ErrNotFound)` (must satisfy
   `errors.Is(err, ports.ErrNotFound)`).
5. `List`: read dir, decode each `.json`, sort by `CreatedAt` desc; a corrupt file is an
   **error**, not a skip (loud corruption beats silent data loss).
6. `Update`: `mu.Lock()`; read current (ErrNotFound if absent); apply `fn` to a *copy*;
   verify `copy.ID == id` after fn (error otherwise); atomic write; return copy. fn runs
   under the lock — document "fast, no I/O, no store calls" on the method.
7. **Atomic writes everywhere:** write to `<file>.tmp-<rand>` in the same directory,
   `chmod 0600`, then `os.Rename`. On any error remove the temp file. (Same-directory rename
   is atomic on APFS; this also gives crash safety.)
8. JSON: `json.MarshalIndent(order, "", "  ")`. File perms `0600` (health data). Dirs `0750`.
9. `Blobs.Put`: mkdirall `blobs/<id>/<kind>` (0750), stream `io.Copy` to temp file, rename.
10. `Blobs.Open`: `os.Open`; missing → wrap `ports.ErrNotFound`. `Blobs.Delete`: `os.Remove`,
    missing → nil (idempotent per the port contract).
11. One `sync.RWMutex` for the whole store is fine at prototype scale — note this in a
    comment; per-key sharding is a later optimization, not now.

Verify: `go build ./...`; write `local_test.go` (see §6) and `go test -race ./internal/adapters/storage/local/`.

### Step 6 — `internal/adapters/stub`

Package `stub` = deterministic fake providers. **Two roles:** (a) make the C0 binary fully
wired without external services, (b) permanent test doubles for core tests in C3+. Each
adapter takes an optional `*slog.Logger` (nil → discard). Keep them dumb and synchronous —
no goroutines except where the port contract requires stream delivery. Mark every type with
the doc comment: `// Fake provider for development and tests; not for production.`

- **`telephony.go`** — `type Telephony struct{}`, `Dial(ctx, order)`:
  - validate `order.Clinic.Phone != ""` (error otherwise);
  - buffered channels (cap 16);
  - spawn one goroutine that emits `EventDialing` → `EventRinging` → `EventAnswered` on
    `events` and then waits for the session to be hung up;
  - `Hangup` emits `EventEnded{EndReason: EndCompleted}` and closes `events` + `inbound`;
    it must be idempotent and safe from two goroutines (`sync.Once`);
  - `Inbound` delivers one `AudioChunk` of `FrameBytes` zero bytes after `answered`
    (silence, canonical format) — enough for a future loop to see audio plumbing work;
  - `SendAudio`/`SendDTMF` succeed and discard (DTMF digits go to a debug log line).
- **`stt.go`** — `TranscribeStream` returns a stream: `Feed` counts chunks and returns nil;
  `Results()` yields nothing until `Close`; `Close` emits exactly one
  `TranscriptionEvent{Text: fmt.Sprintf("stub heard %d chunk(s)", n), Final: true, At: time.Now()}`
  and closes the channel. `TranscribeFile` returns
  `domain.Transcript{Text: "stub file transcript"}`.
- **`tts.go`** — `Speak` returns a stream; each `FeedText` call emits exactly one
  `AudioChunk` of `domain.FrameBytes` zero bytes (placeholder silence) on `Audio()`;
  `Close` closes the channel. Deterministic: N feed calls → N chunks.
- **`dialogue.go`** — `NewSession` stores the request; `SendUserTurn` returns a turn whose
  `TextChunks()` streams the words of `"stub: <echo of input>"` and whose `Wait` returns an
  empty `TurnResult` (no tools, no outcome). `SubmitToolResults` returns the same shape.
  `Close` → nil.

Verify: `go build ./...`; write `stub_test.go` (see §6).

### Step 7 — `internal/app` (composition root)

```go
package app

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/artemidius/insteadly/internal/adapters/storage/local"
	"github.com/artemidius/insteadly/internal/adapters/stub"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/core/ports"
)
```

```go
// App is the fully wired object graph. It is the ONLY type that knows concrete
// adapters (dependency inversion, DESIGN.md §1.2).
type App struct {
	Config    config.Config
	Storage   ports.Storage
	Telephony ports.Telephony
	STT       ports.SpeechToText
	TTS       ports.TextToSpeech
	Dialogue  ports.Dialogue
	Payments  ports.Payments  // nil until C6
	Notify    ports.Notifier  // nil until C7
	Log       *slog.Logger
}

func New(cfg config.Config) (*App, error) {
	log := newLogger(cfg.Log)

	storage, err := buildStorage(cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	telephony, err := buildTelephony(cfg.Telephony)
	if err != nil {
		return nil, fmt.Errorf("telephony: %w", err)
	}
	stt, err := buildSTT(cfg.STT)
	if err != nil {
		return nil, fmt.Errorf("stt: %w", err)
	}
	tts, err := buildTTS(cfg.TTS)
	if err != nil {
		return nil, fmt.Errorf("tts: %w", err)
	}
	dialogue, err := buildDialogue(cfg.Dialogue)
	if err != nil {
		return nil, fmt.Errorf("dialogue: %w", err)
	}
	return &App{
		Config: cfg, Storage: storage, Telephony: telephony,
		STT: stt, TTS: tts, Dialogue: dialogue, Log: log,
	}, nil
}
```

Builders are one switch each:

```go
func buildTelephony(c config.Telephony) (ports.Telephony, error) {
	switch c.Provider {
	case "stub":
		return stub.NewTelephony(nil), nil
	case "twilio":
		return nil, fmt.Errorf("provider %q: %w (lands in chunk C1)", c.Provider, ports.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("unknown provider %q", c.Provider)
	}
}
```

(`buildSTT`/`buildTTS`/`buildDialogue` identical in shape with `mistral` → ErrNotImplemented,
C2; `buildStorage` only accepts `local`.)

`newLogger`: `slog.NewTextHandler`/`JSONHandler` on `os.Stderr` with level parsed from cfg;
unknown level can't happen post-Validate but guard anyway.

Verify: `go build ./...`.

### Step 8 — `internal/httpapi`

**`server.go`**:

```go
package httpapi

// New builds the HTTP server. cfg.HTTP.Addr is bound by the caller (main).
func New(cfg config.Config, a *app.App, version string) *http.Server
```

Routes (Go 1.22 method patterns; no router dependency):

| Method | Path | Handler | Success | Errors |
|---|---|---|---|---|
| GET | `/healthz` | health | 200 | — |
| GET | `/api/v1/orders` | list | 200 `[]` | — |
| POST | `/api/v1/orders` | create | 201 + order | 400, 409, 413, 500 |
| GET | `/api/v1/orders/{id}` | get | 200 + order | 404 |

- `mux.HandleFunc("GET /healthz", ...)` etc. — ServeMux returns 405 automatically for wrong
  methods; leave that default.
- Middleware chain (outermost first): `recoverer` → `requestID` → `logger`. recoverer must
  convert panics to `500 {"error":{"code":"internal","message":"internal error"}}` and log
  with stack (`debug.Stack()`). requestID: honor inbound `X-Request-Id`, else generate
  (reuse `domain.NewID`-style helper — but do not import domain for this; a small local
  `newRequestID()` in httpapi using crypto/rand is fine). logger: one slog line per request —
  method, path, status, duration_ms, request_id — via a `statusRecorder` wrapping
  `http.ResponseWriter`.
- JSON contract everywhere:
  - success: direct JSON of the resource; order JSON tags are on the domain structs;
  - error envelope: `{"error":{"code":"<snake_case>","message":"<human string>"}}`;
  - map errors: `ports.ErrNotFound` → 404 `not_found`; `ports.ErrConflict` → 409 `conflict`;
    `*http.MaxBytesError` → 413 `payload_too_large`; decode/validation → 400 `bad_request`;
    anything else → 500 `internal` (log the real error, never leak it to the client).

**`orders.go`** — handlers:

- `createOrder`:
  - `http.MaxBytesReader(w, r.Body, 1<<20)` (1 MiB);
  - decode with `json.NewDecoder` + `DisallowUnknownFields()` (strict; unknown field → 400);
  - reject non-empty `id` (server assigns);
  - reject any `status` other than "" or "created" (the API cannot mint paid orders —
    payments do that in C6);
  - validate: `clinic.name` and `clinic.phone` non-empty, `patient.full_name` non-empty;
    `birth_date` if present matches `^\d{4}-\d{2}-\d{2}$`; each `preferred_slots` range has
    `start.Before(end)`. Collect ALL violations into one 400 response:
    `{"error":{"code":"bad_request","message":"field \"clinic.name\" is required"}}`.
  - `storage.Orders().Create` → 201 + JSON order. `Location: /api/v1/orders/<id>` header.
- `listOrders` → `storage.Orders().List()` → 200 JSON array (`[]`, never `null` — initialize
  the slice).
- `getOrder` → `r.PathValue("id")` → `Get`; ErrNotFound → 404 envelope.
- `healthz` → 200:
  ```json
  {
    "status": "ok",
    "version": "<version>",
    "uptime_seconds": 12,
    "providers": {
      "telephony": "stub", "stt": "stub", "tts": "stub",
      "dialogue": "stub", "storage": "local"
    }
  }
  ```
  (provider names from cfg, `uptime` from a `time.Time` captured in `New`. Never include
  secrets.)

Verify: `go build ./...`; write `server_test.go` (see §6).

### Step 9 — `cmd/insteadly/main.go`

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/artemidius/insteadly/internal/app"
	"github.com/artemidius/insteadly/internal/core/config"
	"github.com/artemidius/insteadly/internal/httpapi"
)
```

```go
// version is set at build time: -ldflags "-X main.version=..."; dev otherwise.
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("insteadly exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a, err := app.New(cfg)
	if err != nil {
		return err
	}
	a.Log.Info("starting insteadly", "config", cfg.Summary(), "version", version) // Summary: secrets never logged

	srv := httpapi.New(cfg, a, version)

	errCh := make(chan error, 1)
	go func() {
		a.Log.Info("http listening", "addr", cfg.HTTP.Addr)
		errCh <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		a.Log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}
```

`httpapi.New` returns `*http.Server` with `ReadHeaderTimeout: 5 * time.Second` — set it there.

### Step 10 — Full verification (exit criteria, see §7)

---

## 5. Conventions (apply everywhere, no exceptions)

1. **Errors:** wrap with `%w` at every boundary; sentinels from `ports`/`domain` only;
   lowercase messages; never log secrets; client-facing messages never contain internal
   paths, SQL, or provider details.
2. **Logging:** `log/slog` exclusively; structured key-value pairs (snake_case keys);
   every handler log line carries `request_id`; no `fmt.Println` in production code.
3. **gofmt** on every file before committing anything; `make lint` must be clean.
4. **Comments:** doc comments on every exported identifier (ports *require* the contract
   prose in their comments); inline comments only for non-obvious invariants (e.g. atomic
   rename, lock scope, canonical audio).
5. **No new dependencies.** C0 uses stdlib only (`go.mod` must have zero `require` lines).
   If you think you need a dependency (router, godotenv, etc.) — stop; the plan says no.
6. **JSON field names snake_case**; timestamps RFC3339 (Go default); enums as strings
   exactly as defined in `domain`.
7. **Never touch `.env`**; never read its values except via config (C0 doesn't read them
   at all). Never print or log `TWILIO_*`/`MISTRAL_*` values.

## 6. Test plan (file → cases)

| Test file | Cases (table-driven where sensible) |
|---|---|
| `domain/order_test.go` | every legal transition; every illegal transition errors with `ErrInvalidTransition`; `Transition` to `finished` rejected; `Finish` with each outcome kind (booked needs slot; callback needs time; failed needs reason; aborted bare; unknown kind errors); `Finish` on finished order errors; `Finish` sets status+outcome; `Terminal()` truth table |
| `config/config_test.go` | defaults (providers=stub, addr, dir); `.env` file (temp dir fixture) is applied; process env wins over `.env`; unknown `INSTEADLY_*` errors; enum violations error (`telephony.provider=foo`, bad log level); bad bool errors; `Summary()` contains no secret values (assert strings for known secret keys absent — but note: test cannot know real secrets, so assert on the *keys*); unset everything → defaults |
| `storage/local/local_test.go` | create assigns ID + timestamps; create with existing ID → `ErrConflict`; get missing → `ErrNotFound` (via `errors.Is`); update applies fn and persists; update on missing → `ErrNotFound`; update changing ID → error; list returns newest-first; list of empty store returns empty slice not nil; blob put/open roundtrip (bytes equal); blob open missing → `ErrNotFound`; blob delete idempotent; path traversal attempts rejected: id `"../../etc/passwd"`, kind `"../../x"`, name `"../x"` and `"/abs"`; concurrent updates (10 goroutines bumping a counter via Update) end consistent under `-race` |
| `stub/stub_test.go` | telephony: event sequence dialing→ringing→answered on `Events()`, hangup → ended + channel close, double-hangup safe, SendAudio/SendDTMF nil; stt: N feeds → close → exactly one final event with "stub heard N chunk(s)"; tts: N feeds → N chunks of `FrameBytes` zeros, close closes channel; dialogue: text chunks echo input, Wait returns empty TurnResult |
| `httpapi/server_test.go` | build app via `app.New(config with local storage in t.TempDir())` + `httptest.NewServer(httpapi.New(...))`: healthz 200 + `status:ok` + providers; create 201 + `Location` header + echo fields; create missing name → 400 envelope with `code:"bad_request"`; create unknown JSON field → 400; create with client-set id → 400; create with non-created status → 400; get by id → 200; get unknown id → 404 envelope `not_found`; list → array containing created order; body > 1 MiB → 413; wrong method on POST-only route → 405 |

Run `make test` (includes `-race`), `make lint`, `make check-deps`, `make build` — all green.

## 7. Definition of done (exit criteria — record evidence in PROGRESS.md)

1. `make lint` clean; `make test` green (`-race`); `make build` produces `bin/insteadly`;
   `make check-deps` prints `OK: core imports no adapters`.
2. `./bin/insteadly` boots with **no env vars set** (defaults) and logs the config summary.
3. Smoke test against the live binary:
   ```bash
   ./bin/insteadly &
   curl -s http://127.0.0.1:8080/healthz | jq .status           # "ok"
   curl -s -X POST http://127.0.0.1:8080/api/v1/orders \
     -H 'Content-Type: application/json' \
     -d '{"clinic":{"name":"Praxis Test","phone":"+431234567"},
          "patient":{"full_name":"Max Mustermann","phone":"+436641234567",
                     "reason":"test"}}' | jq .id                 # 32-hex id, status "created"
   curl -s http://127.0.0.1:8080/api/v1/orders | jq 'length'    # ≥ 1
   curl -s http://127.0.0.1:8080/api/v1/orders/<id> | jq .status # "created"
   ls data/orders/                                             # <id>.json exists
   kill %1
   ```
4. Negative check: `INSTEADLY_TELEPHONY_PROVIDER=twilio ./bin/insteadly` exits non-zero with
   a clear "not implemented (chunk C1)" error; `INSTEADLY_STORAGE_PROVIDER=bogus` fails
   validation; `INSTEADLY_NOPE=1 ./bin/insteadly` fails strict-mode with "unknown variable".
5. `go.mod` has zero external requires; `git status` shows only the files in §3 (+ test files);
   `.env`, `data/`, `bin/` are untracked/ignored.
6. PROGRESS.md updated per its protocol (C0 → done, evidence, session log entry).

## 8. Expected size & effort

Roughly **1,200–1,600 lines** including tests; one focused session. The slow part is not
typing — it is keeping the contracts *exactly* as specified (they are the seams C1–C7 plug into).
## 9. What C1–C7 will plug in here (do not build now, but leave room)

- **C1 (Twilio):** `internal/adapters/telephony/twilio/` implements `ports.Telephony` +
  `ports.CallSession` (µ-law→PCM16, webhooks, Media Streams WS). Wiring: one new case in
  `app.buildTelephony`.
- **C2 (Mistral):** `internal/adapters/{stt,tts,dialogue}/mistral/` implement the three AI
  ports. Wiring: three new cases.
- **C3 (agent loop):** new `internal/core/agent/` consuming Telephony+STT+TTS+Dialogue and
  writing outcomes via the state machine; `internal/core/runner/` per-call goroutines.
- **C5 (orchestration):** `internal/core/scheduler/`, Postgres storage adapter, SSE endpoint
  in `httpapi`.
- **C6:** web app + `Payments` adapter; **C7:** `Notifier` adapter + second providers.

