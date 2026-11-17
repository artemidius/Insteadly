# C7 — Swapability proof + hardening (M4): second telephony adapter, alternative AI provider, recording/storage flags, notifications

> Status: plan (read-only once implementation starts; amend via dated `## 14. Amendments` only).
> Contract source of truth: `DESIGN.md` §1.2 (swap-anytime promise), §1.5 (canonical audio),
> §2.2 (ports table), §3.4 (recording flags), §5 (M4), §6 (Q5 deployment, Q7 DialogueEngine).
> On conflict, DESIGN wins — stop and flag, don't guess.
> Seams inherited: `plans/07-web-app.md` §13.3 (`Notifier`, `WindowProvider`, River, CSRF/reset/rate-limit, deployment).

---

## 1. What this chunk is

C7 is the **M4 — swapability & production hardening** chunk, and the final roadmap chunk.
It proves the DESIGN §1.2 "swap-anytime" promise with **zero core changes** by adding a
**second adapter per capability**, and it closes the two hardening gaps every earlier chunk
deliberately deferred: **recording/storage flags end-to-end** and **notifications**.

Two facts are established and treated as given, not re-decided:

1. **Every external capability already has a port and a registry entry, and the core never
   imports adapters** (`make check-deps` enforces it; `internal/core/config` is stdlib-only).
   C7 adds *adapters* and *wiring*, not new ports.
2. **The recording flags already exist but are inert**: `config.Policy{RecordingEnabled,
   TranscriptEnabled, AudioEnabled}` + `domain.Order.Recording *RecordingFlags` (per-order
   overrides) are parsed, validated, and surfaced in `Summary`, but nothing persists them —
   recording persistence was explicitly deferred to C7 (`plans/05-conversation.md` §11,
   `plans/06-orchestration.md` §1). C7 wires them to the storage boundary.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | Second telephony adapter: **Telnyx Call Control + Media Streams** (not Twilio; no live call) | `internal/adapters/telephony/telnyx/` |
| 2 | Alternative AI provider: **OpenAI for the Dialogue port** (single port proves swapability) | `internal/adapters/dialogue/openai/` |
| 3 | Recording flag resolution + capture/persist recorder (WAV + transcript via `ports.BlobStore`) | `internal/core/recorder/` |
| 4 | Notifier adapters (`log` + `webhook`) behind the existing `ports.Notifier` | `internal/adapters/notify/{log,webhook}/` |
| 5 | Notify worker: terminal `Outcome` → `Notifier.Send` (when configured), with dedupe | `internal/core/notify/` |
| 6 | Config + wiring: `telnyx`, `openai`, `notify`, recorder injection; secret-safe `Summary` | `internal/core/config/`, `internal/app/` |
| 7 | Fake HTTP/WS/SSE servers for Telnyx + OpenAI; recording-flag matrix; notifier test; swapability integration test | core/adapter tests |
| 8 | Docs + PROGRESS update (implementation session only) | `README.md`, `docs/{telnyx,openai,recording,notifications}.md`, `PROGRESS.md` |

### Out of scope (do NOT implement — later chunks / never in this repo)

- **Any Twilio work or live phone call.** Twilio stays untouched; the second telephony
  adapter is Telnyx, tested only against in-repo fake HTTP/WebSocket servers (no Telnyx
  account, no credentials, no live call). The existing `internal/adapters/telephony/twilio`
  is **not modified**.
- **Real OpenAI/Telnyx credentials or live API traffic.** All provider tests use in-repo
  `httptest` HTTP/SSE and `coder/websocket` loopback servers.
- **Real email/SMS/notification delivery.** The notifier adapters are `log` (no side effect)
  and `webhook` (one HTTP POST); no SMTP/SMS provider or credentials.
- **River / durable outbox / multi-instance notification draining.** The notifier is
  best-effort in-process; a durable outbox is a C8+ seam.
- **A second STT or TTS provider.** One alternative AI port (Dialogue) is enough to prove the
  swap promise (justification in §3.2).
- **`DialogueEngine` coarse port (Q7), per-clinic windows (Q3), CSRF/password-reset/email
  verify/rate limiting, deployment target (Q5).** These stay documented seams, not C7.
- **A Postgres blob store.** Blobs stay on the local adapter (as in C5/C6); C7 only *writes*
  blobs through the existing `ports.BlobStore`.

---

## 2. Preconditions and baseline

- `main` = `f17be88` ("docs: mark C6 code complete"). C0–C6 done; C7 is the final chunk.
- `go 1.27.4`; existing direct deps: `coder/websocket v1.8.15`, `pgx/v5 v5.13.0`,
  `stripe-go/v87 v87.1.0`, `testcontainers-go v0.45.0` (+postgres module), `x/crypto v0.58.0`.
  **C7 adds zero new module dependencies** (§3.4).
- Toolchain: `/usr/bin/make` broken → use `/Library/Developer/CommandLineTools/usr/bin/make`;
  build/test in this sandbox use `/tmp` GOCACHE/GOMODCACHE/GOPATH; `CGO_ENABLED=0` for
  `postgres`-tagged tests.

**Baseline (observed this session):**
```
GOCACHE=/tmp/insteadly-go-cache GOMODCACHE=/tmp/insteadly-go-mod GOPATH=/tmp/insteadly-go \
  go test -count=1 ./...
```
→ **PASS**: every package `ok` (agent 15.5s, app 3.9s, twilio 2.8s, stt/tts/dialogue mistral,
local/stub/stripe/events/runner/scheduler/config/domain/httpapi/web all green;
`internal/adapters/storage/postgres` and `cmd/insteadly` report `[no test files]` by design —
postgres tests are `//go:build postgres`-gated). No failures.

`make check-deps` currently prints `OK: core imports no adapters` and must stay that way after
C7 (the recorder and notify worker are **core** packages that import only `domain`, `ports`,
`events`, and stdlib — never an adapter).

---

## 3. Decisions (decided, with rationale)

### 3.1 Second telephony adapter — Telnyx Call Control + Media Streams (decided)

**Provider:** Telnyx. It is the non-Twilio candidate DESIGN §1.3/§3.1 already names, it offers
a **Call Control REST API + a bidirectional media-stream WebSocket** (so the port contract is
expressible, not just a stub), and its media audio is **G.711 µ-law mono 8 kHz** — the *same*
codec Twilio Media Streams delivers, so the µ-law↔PCM16-16 kHz conversion is already proven in
`twilio/audio.go` and only needs a self-contained copy in the new package.

**Swapability proof stays real, not paper:** the adapter speaks Telnyx's documented protocol,
is selected purely by `INSTEADLY_TELEPHONY_PROVIDER=telnyx`, and is exercised end-to-end by an
in-repo fake server. No Telnyx account or live call is used.

**Exact protocol surface (§5):** `POST /v2/calls` (create), call-control webhooks
(`call.initiated`, `call.answered`, `call.hangup`, `streaming.started/stopped`),
`POST /v2/calls/{id}/actions/streaming_start` (attach the WSS), the bidirectional media stream
(`start` / `media` / `dtmf` / `stop`, base64 µ-law 8 kHz), `POST /v2/calls/{id}/actions/hangup`.
Outbound DTMF is the one surface with a docs-verify caveat (§5.5, §13).

### 3.2 Alternative AI provider — OpenAI for the Dialogue port (decided)

One alternative adapter is enough to prove the swap promise ("a single port is enough"), and
**Dialogue is the cleanest port to prove it on**:

1. **Text-in/text-out, no audio codec.** The `ports.Dialogue` contract is pure text + tools +
   outcome; there is no sample-rate/format surface to get wrong.
2. **OpenAI Chat Completions is SSE over plain HTTP**, the exact shape the existing
   `dialogue/mistral` adapter already parses (`choices[].delta.content`,
   `choices[].delta.tool_calls[].function.{name,arguments}`, `finish_reason`, `[DONE]`). The
   OpenAI adapter is a near drop-in variant: different base URL, model, and
   `max_completion_tokens` field, same SSE/tool/`finish_call` extraction.
3. **Zero new dependencies** — stdlib `net/http` + `bufio` (the Mistral adapter already has no
   SSE library; §6).
4. It is **fake-testable with a single httptest SSE server** (§6.3, §11).

STT/TTS stay on `mistral` (a second *audio* provider adds codec/WebSocket risk for no extra
proof). This is the one place where "one alternative AI port" is exercised; the config
plumbing makes adding `openai` to STT/TTS later a one-line registry change.

### 3.3 Recording capture design (decided)

- **Master gate + two artifact selectors.** `recording.enabled` is the master capture gate;
  `audio.enabled` selects persisting the **call audio (WAV)**; `transcript.enabled` selects
  persisting the **transcript**. Resolution is per-order-over-global (§4.1). Defaults are all
  `false`, which is byte-for-byte today's behavior (nothing captured, nothing persisted).
- **Capture both directions, two mono WAVs.** The recorder tees the **inbound** track (clinic)
  and the **outbound** track (assistant) at the `ports.CallSession` boundary — a single seam in
  the runner — into two in-memory WAV builders. Two mono WAVs avoid stereo-interleave clock
  plumbing and keep "who said what" unambiguous.
- **Transcript via the existing batch STT port.** After the call, the recorder runs
  `ports.SpeechToText.TranscribeFile` over the captured *inbound* PCM and persists the text.
  This reuses the C2 batch-STT path, needs no agent change, and matches DESIGN §2.2 ("batch
  mode for post-call transcripts of recordings").
- **Enforced at the storage boundary.** The flags only decide what is *persisted* via
  `ports.BlobStore`; when off, `Recorder.Wrap` returns the session unchanged and zero bytes are
  written.

The three-way `recording` / `audio` / `transcript` distinction is a **decision to sanity-check**
(§13); the master-gate reading is the common call-recording model and keeps the default-off
contract exact.

### 3.4 New dependencies (none) — plan-level justification

- Telnyx: stdlib `net/http` (REST) + `github.com/coder/websocket` (already present, used by the
  Twilio and Mistral realtime adapters).
- OpenAI: stdlib `net/http` + `bufio` (SSE), exactly like `dialogue/mistral`.
- Recording WAV: stdlib `encoding/binary` (44-byte RIFF/WAVE header + raw PCM16).
- Notifier webhook: stdlib `net/http`.

`go mod tidy` must show **no diff** to `go.mod`/`go.sum` (§10, §12). Any new dep that appears
is a stop-and-justify gate.

---

## 4. Ports → components and exact contracts

**No port changes.** C7 reuses the existing contracts verbatim — `ports.Telephony` /
`ports.CallSession`, `ports.Dialogue` / `ports.DialogueSession`, `ports.SpeechToText`
(`TranscribeFile`), `ports.BlobStore`, `ports.Notifier`, `ports.OrderStore`,
`ports.AuthStore`, `events.{Publisher,Subscriber}`. The new seams are *core types* (not ports)
and *adapters*:

| Capability | Port (unchanged) | First adapter (existing) | Second adapter (C7) | Wiring |
|---|---|---|---|---|
| Telephony | `ports.Telephony` | `telephony/twilio` | `telephony/telnyx` | `buildTelephony` |
| Dialogue | `ports.Dialogue` | `dialogue/mistral` | `dialogue/openai` | `buildDialogue` |
| STT | `ports.SpeechToText` | `stt/mistral` | — (no second adapter) | `buildSTT` |
| TTS | `ports.TextToSpeech` | `tts/mistral` | — | `buildTTS` |
| Storage | `ports.Storage`/`BlobStore` | `storage/local` | `storage/postgres` (C5; blobs stay local) | `buildStorage` |
| Payments | `ports.Payments` | `payments/stub` | `payments/stripe` (C6) | `buildPayments` |
| Notify | `ports.Notifier` | — | `notify/log`, `notify/webhook` | `buildNotifier` |

### 4.1 Recording flag resolution (verbatim, new core package `internal/core/recorder`)

```go
// Package recorder captures one call's audio and, on finalize, persists the WAV and/or the
// post-call transcript through ports.BlobStore, gated by the resolved recording flags. It is
// core: it imports only domain, ports, and stdlib — never an adapter.
package recorder

import (
	"log/slog"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Policy is the global recording defaults, copied from config.Policy by the composition root.
// The recorder never imports internal/core/config.
type Policy struct {
	Recording  bool
	Transcript bool
	Audio      bool
}

// Decision is the resolved per-order recording policy. Capture means "capture audio in
// memory" (required for either artifact); WAV and Transcript are the two storage-boundary
// decisions.
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
// *recorder.Recorder; the runner only sees this interface so it stays testable with a nil or
// fake recorder.
type Recorder interface {
	// Wrap returns a teeing ports.CallSession that captures this order's inbound and
	// outbound canonical audio when the resolved decision says to, or the original session
	// unchanged (and no goroutines) otherwise. It must forward every ports.CallSession
	// method, including the optional StreamReady() <-chan struct{} diagnostic when the
	// underlying session exposes it.
	Wrap(session ports.CallSession, order domain.Order) ports.CallSession
}

// New builds the concrete recorder. stt is used only for TranscribeFile (post-call batch
// transcription); blobs is the storage boundary; log may be nil.
func New(stt ports.SpeechToText, blobs ports.BlobStore, global Policy, log *slog.Logger) *RecorderImpl

// (RecorderImpl, the WAV writer, and the teeing session are package-private; their contract
//  is specified in §7.)
var _ Recorder = (*RecorderImpl)(nil)
```

> The exact receiver/struct names are implementation detail; the *contract* above is what the
> runner and `internal/app` bind to.

### 4.2 Runner seam (one line, additive)

`runner.Config` gains an optional field; `Run` wraps the dialed session once, right after a
successful `Dial` and before the pre-agent loop:

```go
// runner.Config (additive):
Recorder recorder.Recorder // optional; nil = recording disabled (today's behavior)
```

```go
// runner.Run, immediately after `session, err := r.cfg.Telephony.Dial(...)` succeeds:
if r.cfg.Recorder != nil {
	session = r.cfg.Recorder.Wrap(session, claimed)
}
```

This is the **only** runner change for recording, and the **only** runner/agent change in the
whole chunk. It proves the telephony swap and the recording toggle both leave the
`Runner`/agent conversation logic untouched: when flags are off, `Wrap` returns the identical
session and behavior is byte-for-byte unchanged.

### 4.3 Notify worker (verbatim, new core package `internal/core/notify`)

```go
// Package notify turns terminal order outcomes into outbound notifications. It is core: it
// imports domain, ports, and events — never an adapter.
package notify

import (
	"context"
	"log/slog"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/events"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// Config wires the worker. Users is optional (nil = no email resolution; the recipient falls
// back to the patient phone). Notifier must not be nil.
type Config struct {
	Events   events.Subscriber
	Orders   ports.OrderStore
	Users    ports.AuthStore // optional
	Notifier ports.Notifier
	Logger   *slog.Logger
}

// Worker subscribes to the order-status hub and sends one notification for every finished
// order event (Status == finished, Outcome != nil), deduped by (OrderID, Attempts).
type Worker struct{ /* … */ }

// New validates the wiring and performs no I/O.
func New(cfg Config) (*Worker, error)

// Run blocks until ctx is canceled or the hub closes the subscription. It is the worker's
// main loop; start it from the composition root. Send is best-effort: a Send error is logged,
// never retried infinitely (a durable outbox is a C8+ concern).
func (w *Worker) Run(ctx context.Context) error
```

Recipient resolution (documented, implemented in the worker): if `order.UserID != ""` and
`Users != nil`, `GetUser` → `Channel="email"`, `To=user.Email`; else if `order.Patient.Phone`
is non-empty → `Channel="sms"`, `To=patient.Phone`; else `Channel="email"`, `To=""` (the
`log` notifier still records it, the `webhook` notifier still posts it). The notification
body/subject is a fixed, non-secret summary of the outcome (§8.2).

---

## 5. Telnyx telephony adapter (`internal/adapters/telephony/telnyx`)

Provider concepts (`call_control_id`, `connection_id`, `stream_id`, µ-law, webhook signing)
stay inside this package; the core sees only `ports.CallSession` + canonical PCM16.

### 5.1 REST surface (stdlib `net/http`, Bearer auth)

| Operation | Request | Key response |
|---|---|---|
| Create call | `POST {api}/v2/calls` `{"to","from","connection_id","webhook_url"}` | `{"data":{"call_control_id":"…"}}` |
| Attach stream | `POST {api}/v2/calls/{id}/actions/streaming_start` `{"stream_url":"wss://…","stream_track":"both_tracks"}` | 200 |
| Hangup | `POST {api}/v2/calls/{id}/actions/hangup` | 200 (422 code `90018` = already ended) |

`Config.APIBaseURL` (default `https://api.telnyx.com`) is the test-only override, exactly like
the Twilio adapter's `APIBaseURL`. `Authorization: Bearer <api_key>`; `Content-Type:
application/json`.

### 5.2 Call-control webhooks (adapter-owned HTTP handler)

The adapter exposes `Handler() http.Handler` mounted by `httpapi` under
`/webhooks/telephony/telnyx/` (mirroring the Twilio mount). Endpoints:
`POST /webhook/{callID}` (call-control events: `call.initiated`, `call.answered`,
`call.hangup`, `streaming.started`, `streaming.stopped`) and `GET /stream/{callID}` (the WSS
upgrade). Event → `domain.CallEvent` normalization mirrors `twilio/session.go`: `initiated` →
`dialing`, `answered` → `answered`, `hangup` → `ended` (reason from the event's
`hangup_cause`/`hangup_source` mapped to `EndBusy`/`EndNoAnswer`/`EndRejected`/`EndError`/
`EndCompleted`).

**Webhook auth:** the unguessable per-call `{callID}` path + a `call_control_id`/`connection_id`
field match is the capability check (Telnyx, like Twilio's unsigned voice fetch, does not sign
every callback); when an `X-Telnyx-Signature` header is present it must verify (HMAC-SHA256 of
the raw body using the configured signing key). The exact signature algorithm is a
docs-verify-at-implementation item (§5.5) — fail-closed if present and unverifiable.

### 5.3 Media stream WebSocket (bidirectional; `coder/websocket`)

The adapter is the **server** (Telnyx connects to `stream_url`), matching the Twilio model.
Inbound messages (validated, then normalized):

| Inbound `event` | Payload | Action |
|---|---|---|
| `start` | `{"stream_id":"…"}` | attach stream; signal `StreamReady()`; `progress(answered)` |
| `media` | `{"media":{"payload":"<base64>"}}` | base64-decode, µ-law→PCM16-16 kHz (own codec copy), `pushInbound` |
| `dtmf` | `{"dtmf":{"digit":"0-9A-D*#"}}` | `EventDTMF` |
| `stop` | — | detach; no terminal event by itself |

Outbound messages (ordered, single writer): `{"event":"media","media":{"payload":"<base64>"}}`
and `{"event":"clear"}` (barge-in). Playback **marks**: Telnyx's media stream has no mark echo;
the adapter emits `EventPlaybackMark` locally when the outbound writer drains a queued mark
item after all prior media (same "local mark = provider reached that point, not proof a human
heard it" semantics as the `stub` adapter), and `ClearAudio` maps to `clear`.

### 5.4 Audio codec (self-contained copy)

`telnyx/audio.go` re-implements the standard G.711 µ-law ↔ PCM16 16 kHz conversion and the
8↔16 kHz resampling already proven in `twilio/audio.go` (µ-law decode table + `upsample8to16`,
PCM16→µ-law + anti-alias `downsample16to8`, 160-byte µ-law frame ⇄ 640-byte canonical frame).
It is a **copy, not an import** — adapters do not import each other, and the core must never
depend on it. ~90 lines, fully unit-tested round-trip.

### 5.5 Outbound DTMF (the one docs-verify caveat)

Telnyx's media-stream WS (per the pipecat serializer and the Call Control action list) has **no
outbound DTMF message** — only inbound `dtmf`. The adapter's `SendDTMF` therefore POSTs the
documented Call Control **send-DTMF** action if the current API exposes one
(`POST /v2/calls/{id}/actions/send_dtmf` with `{"digits":"…"}` is the expected shape, verified
against the fetched OpenAPI during implementation). If that action does not exist, `SendDTMF`
returns `ErrDTMFUnsupported` (the same contract the Twilio media-stream-only path already
documents). **Either way C7 is not blocked**: the fake-server test exercises whichever REST
action the adapter actually sends, and the swapability integration test uses a flow that does
not require outbound DTMF. Any deviation from the expected endpoint is a dated §14 amendment.

### 5.6 Fake-server test strategy (no account, no live call)

- **REST fake** (`httptest.Server`): handles `POST /v2/calls` (returns a fake
  `call_control_id`), `POST /…/streaming_start`, `POST /…/hangup`; asserts Bearer header and
  body; records calls.
- **Webhook/WS fake**: `httptest.NewServer(adapter.Handler())` + the test acts as the WS
  *client* (via `coder/websocket`) to `GET /stream/{callID}`; it sends `start`, base64 µ-law
  `media` frames, `dtmf`, `stop` and asserts the adapter's outbound `media`/`clear` frames and
  the normalized `domain.CallEvent`/inbound-audio stream. (This is the same loopback WS pattern
  the Twilio adapter tests already use.)
- A higher-level `Dial` test drives REST-create + a `call.answered` webhook POST + the WS
  session end-to-end.

---

## 6. OpenAI dialogue adapter (`internal/adapters/dialogue/openai`)

Structurally parallel to `dialogue/mistral` (same package layout: `adapter.go`, `session.go`,
`stream.go`, `outcome.go`, `sse.go`, `protocol.go`).

### 6.1 Protocol surface

- `POST {base}/chat/completions` (base default `https://api.openai.com/v1`);
  `Authorization: Bearer <api_key>`; `Content-Type: application/json`; `Accept:
  text/event-stream`.
- Request body = the OpenAI Chat Completions shape: `model`, `messages`, `tools`
  (`{type:"function", function:{name, description, parameters}}`), `tool_choice:"auto"`,
  `parallel_tool_calls:false`, `stream:true`, `temperature:0.1`, and `max_completion_tokens`
  (the field name for current OpenAI models — verify against the API reference; the Mistral
  adapter uses `max_tokens`, which OpenAI deprecates for newer models).
- SSE response = the OpenAI-compatible shape the Mistral adapter already parses:
  `choices[].delta.{role,content,tool_calls[]}`, `choices[].finish_reason`, `[DONE]`.
  `finish_reason` values `stop`/`tool_calls` valid; `length` → `ErrTurnTruncated`.
- `finish_call` reserved-tool extraction and `decodeFinishCall` are **identical semantics** to
  `dialogue/mistral` (`outcome.go`), so the booking outcome contract is unchanged.

### 6.2 Config surface

`config.OpenAI{APIKey, BaseURL, DialogueModel}` → env `INSTEADLY_OPENAI_API_KEY` (secret),
`INSTEADLY_OPENAI_BASE_URL` (default `https://api.openai.com/v1`), `INSTEADLY_OPENAI_DIALOGUE_MODEL`
(default `gpt-4o-mini`). The adapter reuses the same loopback-http-guard rule as Mistral (an
`http` base URL is allowed only for loopback hosts, so a test can point at `httptest` while a
real key is never sent over plaintext).

### 6.3 Fake-server test strategy (no key)

A single `httptest` SSE server responds to `POST /chat/completions` with a scripted
`data: …\n\n` stream: text deltas, a fragmented tool call, a `finish_call`, `[DONE]`. Tests
assert the request body (`model`, `tools` incl. the reserved `finish_call`, `stream:true`), the
streamed `TextChunks`, the assembled `ToolCall`s, and the decoded `Outcome`; plus
interrupt/control/truncation/error cases. **Zero network, zero key.**

---

## 7. Recording/storage flags end-to-end

### 7.1 Capture points (exact)

- **Inbound (clinic) audio** — `ports.CallSession.Inbound()`. The teeing session wraps the
  underlying channel with a relay goroutine that copies every canonical chunk into the inbound
  WAV builder *and* forwards it unchanged. The relay observes the underlying channel close
  (the port guarantees close at call end) and triggers finalize.
- **Outbound (assistant) audio** — `ports.CallSession.SendAudio()`. The teeing session appends
  each non-empty canonical chunk to the outbound WAV builder (mutex-guarded), then forwards to
  the underlying session.
- **Transcript** — produced at finalize by `ports.SpeechToText.TranscribeFile(ctx, rawPCM,
  BatchOptions{Language: "de", Timestamps: false, Diarization: false})` over the captured
  **inbound raw PCM** (not the WAV-with-header). The adapter's batch mode wraps raw PCM in a
  WAV multipart itself (`stt/mistral/batch.go`), so the recorder must feed raw PCM16.

Neither the agent's listener nor the speaker is modified: the tees live entirely in the
session wrapper.

### 7.2 WAV format and blob keys

- Format: **RIFF/WAVE, PCM16 LE mono 16 kHz**, 44-byte header + raw PCM (stdlib
  `encoding/binary`; same header as `stt/mistral/wav.go`, re-implemented in core because the
  core must not import an adapter).
- Blob keys (`domain.BlobKey`, validated by the local store's traversal guard):

| Artifact | Kind | Name |
|---|---|---|
| Inbound recording | `domain.BlobKindRecording` | `<callID>-inbound.wav` |
| Outbound recording | `domain.BlobKindRecording` | `<callID>-outbound.wav` |
| Transcript | `domain.BlobKindTranscript` | `<callID>-transcript.txt` |

`<callID>` = `string(session.ID())` (32 lowercase hex, a valid blob name). `OrderID` = the
order's ID, so retries (distinct call IDs) never overwrite a prior attempt's blobs.

Transcript text:
```
insteadly transcript
order=<orderID>
call=<callID>
clinic=<clinic name>
outcome=<outcome kind>
---
<TranscribeFile().Text>
```

### 7.3 Enable/disable enforcement + per-order override resolution

- Resolve once per `Dial` from `config.Policy` (global) + `order.Recording` (`*RecordingFlags`,
  nil = global; each `*bool` field nil = fall through) — `recorder.Resolve` (§4.1).
- `Decision.Capture == false` → `Wrap` returns the original session unchanged, no goroutine, no
  WAV builder, no transcription. **Default-off behavior is byte-for-byte preserved.**
- `Decision.WAV` → persist both mono WAVs via `blobs.Put` at finalize.
- `Decision.Transcript` → run `TranscribeFile` then persist the text via `blobs.Put`.
- Finalize is `sync.Once`-guarded, idempotent, uses `context.WithTimeout(context.WithoutCancel(…))`,
  and is **best-effort**: a blob/STT error is logged, never fails the call or the runner's
  terminal persist.

### 7.4 Override resolution matrix (the test matrix §11 encodes this)

| global | per-order | effective |
|---|---|---|
| rec=false | nil | capture nothing |
| rec=true, audio=true, tscript=false | nil | WAV only |
| rec=true, audio=false, tscript=true | nil | transcript only (capture in memory, discard audio) |
| rec=true, audio=true, tscript=true | nil | WAV + transcript |
| rec=false | `{Recording:&true, Audio:&true}` | WAV (per-order overrides global) |
| rec=true, audio=false | `{Audio:&true}` | WAV (field-level override) |
| rec=true, audio=true | `{Audio:&false}` | transcript only (if transcript=true) else nothing |

---

## 8. Notifications

### 8.1 Adapters (`internal/adapters/notify/…`)

- **`log`** (default-when-configured, zero side effect): `Send` logs the `domain.Notification`
  (order id, channel, recipient, subject) at `Info`, never the body verbatim if it could carry
  patient detail (log a redacted body). `New(log *slog.Logger)`.
- **`webhook`**: `Send` POSTs the notification as JSON to `config.Notify.Webhook.URL` with a
  bounded timeout; non-2xx → retryable error returned to the caller (the worker logs it).
  `New(url, channel string, log *slog.Logger)`. No signatures in v1 (a webhook secret is a
  C8 seam); the URL is a secret in config and redacted from `Summary`.

Both implement the existing `ports.Notifier` verbatim — **no port change**.

### 8.2 Trigger, dedupe, and format

- **Trigger:** the `internal/core/notify.Worker` subscribes to the existing `events.Hub`
  (`events.Subscriber`) and reacts to `OrderEvent{Status: finished, Outcome != nil}` — the exact
  "terminal Outcome event" the runner already publishes after `persist`. No runner change for
  notifications.
- **Dedupe:** keyed `(OrderID, Attempts)`; a bounded map prunes old keys. Each
  `(order, attempt)` finishes at most once, so this guards only against a replayed/hypothetical
  duplicate publish.
- **Format:** a fixed, non-secret subject/body from the outcome — `booked` → "Termin bestätigt"
  + date/doctor/practice; `callback_needed` → callback time; `failed` → reason; `aborted` →
  aborted. The `domain.Notification` already carries `Channel/To/Subject/Body`.

### 8.3 Gating and lifecycle

`notify.provider=off` (default) → no notifier, no worker, `App.Notify == nil` — current
behavior. Otherwise `App.Start` starts the worker; `App.Close` stops it (cancel its ctx)
**before** the hub closes, so it drains any final events.

---

## 9. Config and environment surface

### 9.1 New/changed `INSTEADLY_*` keys

| Variable | Default | Values / notes |
|---|---|---|
| `INSTEADLY_TELEPHONY_PROVIDER` | `stub` | `stub`, `twilio`, **`telnyx`** |
| `INSTEADLY_TELNYX_API_KEY` | — | **secret**; required when provider is `telnyx` |
| `INSTEADLY_TELNYX_CONNECTION_ID` | — | Call Control connection/app id; required when `telnyx` |
| `INSTEADLY_TELNYX_FROM_NUMBER` | — | caller ID, E.164; required when `telnyx` |
| `INSTEADLY_TELNYX_RING_TIMEOUT_SECONDS` | `0` | 0 = Telnyx default, else 5..600 (mirrors Twilio) |
| `INSTEADLY_DIALOGUE_PROVIDER` | `stub` | `stub`, `mistral`, **`openai`** |
| `INSTEADLY_OPENAI_API_KEY` | — | **secret**; required when `dialogue.provider=openai` |
| `INSTEADLY_OPENAI_BASE_URL` | `https://api.openai.com/v1` | absolute **https** origin |
| `INSTEADLY_OPENAI_DIALOGUE_MODEL` | `gpt-4o-mini` | |
| `INSTEADLY_NOTIFY_PROVIDER` | `off` | `off`, `log`, `webhook` |
| `INSTEADLY_NOTIFY_WEBHOOK_URL` | — | **secret**; required when `notify.provider=webhook` |
| `INSTEADLY_NOTIFY_WEBHOOK_CHANNEL` | `email` | `email`, `sms` |

`INSTEADLY_RECORDING_ENABLED` / `INSTEADLY_TRANSCRIPT_ENABLED` / `INSTEADLY_AUDIO_ENABLED`
already exist and become *effective* (§7). STT/TTS provider enums are unchanged.

### 9.2 Config struct additions (verbatim shape)

```go
// config.Telephony gains Telnyx; config.Dialogue enum gains "openai"; new config.Notify.
type Telnyx struct {
	APIKey            string // secret
	ConnectionID      string
	FromNumber        string
	RingTimeoutSeconds int
}

type OpenAI struct {
	APIKey        string // secret
	BaseURL       string
	DialogueModel string
}

type Notify struct {
	Provider string // "off" | "log" | "webhook"
	Webhook  NotifyWebhook
}

type NotifyWebhook struct {
	URL     string // secret
	Channel string // "email" | "sms"
}
```

Validation: `telnyx` requires `api_key`, `connection_id`, E.164 `from_number`, and
`http.public_url` (webhooks), mirroring `validateTwilio`; `openai` requires `api_key` and an
https `base_url` origin (reuse the `validateMistralBaseURL` pattern); `webhook` requires a
non-empty `url` and a valid channel. `dialogueProviders = {"mistral","stub","openai"}` while
`stt`/`tts` keep `aiProviders = {"mistral","stub"}`.

### 9.3 Secret-safe `Summary`

Add `telephony.telnyx.{connection_id,from_number,ring_timeout_seconds}`,
`dialogue.provider` (already), `openai.base_url`, `openai.dialogue_model`,
`notify.provider`, `notify.webhook.channel`; redact `telnyx.api_key`, `openai.api_key`, and
`notify.webhook.url` (never values). `telnyx.connection_id` is logged (it is an id, not a
credential), matching the existing treatment of `twilio.account_sid`/`stripe.price_id`.

---

## 10. Implementation steps (in this order; each has a verify command)

> Run with `/Library/Developer/CommandLineTools/usr/bin/make` and, in this sandbox,
> `GOCACHE=/tmp/insteadly-go-cache GOMODCACHE=/tmp/insteadly-go-mod GOPATH=/tmp/insteadly-go`.

1. **Config:** add `Telnyx`/`OpenAI`/`Notify` structs, registry entries, enum splits,
   validation, `Summary` redactions. Verify: `go test -count=1 ./internal/core/config`.
2. **Recorder core:** `internal/core/recorder` (`Resolve`, WAV writer, teeing session). Verify:
   `go test -count=1 ./internal/core/recorder`.
3. **Runner seam:** add `Recorder` to `runner.Config`, wrap after `Dial`. Verify:
   `go test -count=1 ./internal/core/runner ./internal/app`.
4. **Telnyx adapter:** `client.go`, `webhook.go`, `stream.go`, `session.go`, `audio.go` (+tests).
   Verify: `go test -count=1 ./internal/adapters/telephony/telnyx`.
5. **OpenAI dialogue:** `adapter.go`, `session.go`, `stream.go`, `outcome.go`, `sse.go`,
   `protocol.go` (+tests). Verify: `go test -count=1 ./internal/adapters/dialogue/openai`.
6. **Notifier adapters:** `notify/log`, `notify/webhook` (+tests). Verify:
   `go test -count=1 ./internal/adapters/notify/...`.
7. **Notify worker:** `internal/core/notify` (+tests). Verify: `go test -count=1 ./internal/core/notify`.
8. **Wiring:** `providers.go` (`buildTelephony` telnyx case, `buildDialogue` openai case,
   `buildNotifier`, `buildRecorder`), `app.go` (inject recorder into runner, notifier into
   `App.Start`/`App.Close`), health names `telnyx`/`openai`/`notify`. Verify:
   `go test -count=1 ./internal/app`.
9. **Integration + matrix tests:** `internal/app` swapability test (telnyx + openai + recording
   + notify against fake servers), recording-flag matrix. Verify:
   `go test -race -count=1 ./...`.
10. **Full gates:** `/Library/Developer/CommandLineTools/usr/bin/make verify` (gofmt/vet/race/
    `check-deps`/`build-go`), `CGO_ENABLED=0 go test -tags postgres -count=1 ./...`, and
    `go mod tidy && git diff --exit-code -- go.mod go.sum` (no new deps).
11. **Docs + PROGRESS** (implementation session): `README.md` config table, new
    `docs/telnyx.md` + `docs/openai.md`, `docs/recording.md` + `docs/notifications.md`,
    `docs/architecture.md` provider lists, `PROGRESS.md` session entry.

---

## 11. Test matrix (file → cases)

| File (new) | Cases |
|---|---|
| `internal/core/config/*_test.go` | telnyx/openai/notify env parse, defaults, validation (missing key/connection/public_url, bad E.164, bad base_url/channel), `Summary` redaction (values never present) |
| `internal/core/recorder/resolve_test.go` | full override matrix (§7.4): global-only, per-order nil, field-level `*bool`, master-gate false ⇒ all false |
| `internal/core/recorder/wav_test.go` | header fields (RIFF/WAVE/PCM16/mono/16 kHz), size fields correct, round-trip byte equality |
| `internal/core/recorder/tee_test.go` | inbound/outbound captured verbatim, forward unchanged, finalize writes WAV+transcript, idempotent finalize, nil-capture returns original session, `StreamReady` forwarded |
| `internal/core/runner/*_test.go` (additive) | recorder wrap applied exactly once; nil recorder unchanged; flags-off ⇒ identical session |
| `internal/adapters/telephony/telnyx/*_test.go` | REST create/hangup/streaming_start (bearer + body asserts), webhook event normalization + auth reject, WS `start`/`media`(µ-law→canonical)/`dtmf`/`stop`, outbound `media`/`clear`/mark ordering, codec round-trip, `SendDTMF` action (or `ErrDTMFUnsupported`) |
| `internal/adapters/dialogue/openai/*_test.go` | SSE stream (text/tool/finish_call/truncation), request body shape incl. `max_completion_tokens`, tool assembly, outcome decode, interrupt/control reconciliation |
| `internal/adapters/notify/log/*_test.go` | `Send` logs and returns nil; redacts body |
| `internal/adapters/notify/webhook/*_test.go` | posts JSON to fake server, non-2xx → error, timeout, channel passthrough |
| `internal/core/notify/*_test.go` | finished-with-outcome ⇒ one `Send`; non-finished ignored; dedupe `(order,attempts)`; recipient resolution (user email → patient phone → empty); `Send` error logged not fatal; hub close stops worker |
| `internal/app/swapability_test.go` (new) | boot with `telnyx` + `openai` + recording-on + `notify=log` against in-repo fake servers; drive the existing fake-telephony flow (dial → answered → canonical audio → STT → dialogue → TTS → booked); assert blobs persisted and notifier fired; `make check-deps` green (core untouched) |
| `internal/app/recording_matrix_test.go` (new) | per §7.4: off/off, WAV-only, transcript-only, both, per-order override; assert exactly the expected blobs (and none when off) |

Existing suites (`m2_integration_test.go`, Mistral/Twilio/Stripe fakes) must stay green
unchanged — the strongest "flags off ⇒ behavior unchanged" regression.

---

## 12. Definition of done

**"C7 code complete"** — all of:
- `make verify` → **PASS** (gofmt, `go vet`, `go test -race -count=1 ./...`, `check-deps`
  `OK: core imports no adapters`, Node-free `build-go`).
- `CGO_ENABLED=0 go test -tags postgres -count=1 ./...` → **PASS**.
- `go mod tidy && git diff --exit-code -- go.mod go.sum` → **clean** (zero new deps).
- Swapability integration test boots with `telnyx` + `openai` (+ `postgres` + `stripe` +
  `notify`) against in-repo fake HTTP/WS/SSE servers and completes a call; the telephony swap
  provably requires **zero** `internal/core/{runner,agent}` changes.
- Recording matrix green; notifier test green; flags-off regression green.
- `git diff main -- internal/core/domain internal/core/ports` → **empty** (no port/domain
  contract drift). New core surface is additive (`recorder`, `notify` packages + `runner.Config`
  field).
- No live call, no Twilio change, no Telnyx/OpenAI account or key was used; no secret read or
  logged.

**NOT "done" (explicitly out of scope / blocked, documented not code-complete):**
DESIGN's real-phone M0/M1 exits (Twilio trial), any real Telnyx/OpenAI live call, a durable
notification outbox, per-clinic windows (Q3), River, CSRF/reset/email-verify/rate limiting, and
the Q5 deployment target. These remain the standing blockers recorded in `PROGRESS.md`.

---

## 13. Risks, decision gates, size estimate, C8 seams, sources

### 13.1 Risks / decision gates

1. **Telnyx protocol details (streaming_start body, send-DTMF action, webhook signature).**
   The REST/WS shapes in §5 are from the fetched docs and the pipecat serializer, but the
   developer docs are JS-rendered and truncated. Gate: verify the exact
   `streaming_start`/`send_dtmf`/signature shapes against the fetched OpenAPI **during step 4**
   and record any deviation as a §14 amendment. The fake-server test pins the adapter to what
   is actually implemented, so a wrong guess is caught by tests, not by a live account.
2. **Recording semantics (`recording` as master gate vs three independent flags).** The
   domain already ships three independent `*bool`s; §3.3/§7 reads `recording` as the master
   capture gate. **Sanity-check with the conductor** before step 2 — if "independent flags" is
   intended instead, only `Resolve` changes (drop the master gate), nothing else.
3. **`transcript.enabled` implies in-memory audio capture** even when `audio.enabled=false`
   (you must capture to transcribe). Documented in §7.4 and tested explicitly; flag it in the
   PR so reviewers don't misread it as a flag-enforcement bug.
4. **Hub-subscriber notifier can drop a `finished` event** under a slow notifier (the hub's
   bounded buffer drops slow subscribers). Accepted as best-effort for C7; a durable outbox +
   retry is the C8 seam. The `log` notifier is effectively instant, so the drop is a real risk
   only for `webhook` with a slow endpoint (timeout-bounded).
5. **Wrapping the session must forward the optional `StreamReady()`** or the Twilio readiness
   wait is silently skipped when recording is on. Called out in §4.2 and tested in
   `tee_test.go`.
6. **No new deps is a hard requirement.** Enforce with the `go mod tidy` diff check in §12; if
   `go mod tidy` surfaces anything beyond the existing five direct deps, stop and re-plan
   rather than commit.

### 13.2 Size estimate

Backend ~2,300–2,800 LOC (recorder ~300, runner ~15, config ~180, telnyx ~700, openai ~400,
notify adapters ~130, notify worker ~160, app wiring ~140) plus ~2,000–2,600 LOC of tests.
Comparable to C1+C2 combined; the risky parts are the Telnyx WS/streaming lifecycle and the
recording finalize ordering, not volume.

### 13.3 C8 / later seams (do not build now, but leave room)

- Durable notification outbox + retry (River-aligned), webhook signing secret.
- A second STT/TTS provider (the `openai` dialogue case is the template; `stt`/`tts` enums and
  `buildSTT`/`buildTTS` switches are the only additions).
- Postgres/S3 blob store (recording blobs currently local-only, as in C5/C6).
- Stereo or time-aligned full-call recording, and per-utterance FLAC, if the two-mono-WAV model
  proves insufficient.
- `WindowProvider` (Q3), CSRF/reset/email-verify/rate limiting, Q5 deployment, `DialogueEngine`
  (Q7).

### 13.4 Sources

Repository sources read directly: `DESIGN.md` (§1.2/§1.5/§2.2/§2.3/§3.4/§5/§6), `PROGRESS.md`,
`plans/06-orchestration.md` + `plans/07-web-app.md` (style + §13.3 seams),
`internal/core/ports/{telephony,speech,dialogue,storage,payments,notify,auth,errors}.go`,
`internal/core/domain/{audio,call,order,notify,ids}.go`, `internal/core/{runner,agent}/*`,
`internal/core/events/hub.go`, `internal/core/config/{config,env}.go`,
`internal/app/{app,providers}.go`, `internal/adapters/telephony/twilio/*` (reference adapter),
`internal/adapters/{stt,tts,dialogue}/mistral/*`, `internal/adapters/storage/local/{local,blobs}.go`,
`internal/adapters/{stub,payments/{stub,stripe}}/*`, `Makefile`, `go.mod`, `README.md`,
`docs/{architecture,twilio,mistral,agent,api}.md`.

External sources actually fetched while planning (2026-11-17):

1. [Telnyx — Build a Media Stream Custom Audio Mixer](https://raw.githubusercontent.com/team-telnyx/telnyx-code-examples/main/media-stream-custom-audio-mixer-python/GUIDE.md)
   — Call Control endpoints `POST /v2/calls`, `/actions/{answer,playback_start,streaming_start,
   streaming_stop}`, and webhook events `call.initiated` / `call.answered` / `call.hangup` /
   `streaming.started` / `streaming.stopped`; `streaming_start` body `{"stream_url",
   "stream_track":"both_tracks"}`.
2. [Telnyx — Voice Call Control guide](https://raw.githubusercontent.com/team-telnyx/ai/main/guides/voice-call-control.md)
   — `POST /v2/calls` (`to`, `from`, `connection_id`, `webhook_url`), Bearer auth,
   `call_control_id`, `POST /v2/calls/{id}/actions/hangup`, error codes (`90018` = already
   ended).
3. [pipecat `serializers/telnyx.py`](https://raw.githubusercontent.com/pipecat-ai/pipecat/main/src/pipecat/serializers/telnyx.py)
   — Telnyx media-stream WebSocket JSON: inbound `{"event":"media","media":{"payload":…}}` and
   `{"event":"dtmf","dtmf":{"digit":…}}`; outbound `{"event":"media",…}` and `{"event":"clear"}`;
   audio **PCMU (µ-law) 8 kHz mono** default.
4. [Telnyx — Stream call media over WebSocket](https://developers.telnyx.com/api-reference/websockets/stream-call-media-over-websocket)
   and [Telnyx — Streaming start](https://developers.telnyx.com/api-reference/call-commands/streaming-start)
   (API reference entries; pages JS-rendered/truncated, used only to confirm the endpoints
   exist — the concrete shapes come from sources 1–3).
5. OpenAI Chat Completions streaming shape is the OpenAI-compatible SSE already implemented in
   `internal/adapters/dialogue/mistral/stream.go`; the `max_completion_tokens` vs `max_tokens`
   field is a docs-verify-at-implementation item against
   [OpenAI Chat Completions API reference](https://platform.openai.com/docs/api-reference/chat).

---

## 14. Amendments

### 2026-11-17 — implementation session (C7)

1. **Telnyx webhook signatures are Ed25519, not HMAC-SHA256** (§5.2/§5.5/§13.1 #1).
   Verified against the Telnyx OpenAPI and docs during step 4: every webhook carries
   `telnyx-signature-ed25519` (base64 Ed25519 signature) and `telnyx-timestamp` (Unix
   seconds); the signed message is `{timestamp}|{raw body}`, and the media-stream WebSocket
   handshake signs `{timestamp}|{stream_url}`. Implemented with stdlib `crypto/ed25519` (no
   new dependency), a ±5 minute replay window, base64-or-hex signature decoding, and an
   optional `INSTEADLY_TELNYX_WEBHOOK_PUBLIC_KEY` (base64 raw 32-byte key or PEM). A signed
   request with no configured key is rejected (fail closed), exactly as §5.2 intended.
2. **Media-stream marks are supported by Telnyx** (§5.3). The verified media-streaming
   spec documents outbound `{"event":"mark","mark":{"name":…}}` and a provider echo after
   queued media; `clear` is confirmed. The adapter therefore implements real provider marks
   (echo → `EventPlaybackMark`) instead of the planned local marks. The port contract is
   unchanged.
3. **`streaming_start` needs bidirectional fields** (§5.1/§5.3). Verified
   `StartStreamingRequest`: without `stream_bidirectional_mode=rtp` and
   `stream_bidirectional_codec=PCMU` the default stream is one-way mp3 and outbound `media`
   would not be PCMU. The adapter sends both; `stream_track=both_tracks` as planned.
4. **Verified stream message shapes** (§5.3): `start` carries a top-level `stream_id` plus
   `start.{call_control_id, media_format:{encoding:"PCMU",sample_rate:8000,channels:1}}`;
   `media` frames carry `track` (`inbound`/`outbound`) because `both_tracks` is requested, so
   only `inbound` (or an absent track) is captured; `dtmf.digit`, `mark.name`, `stop`, and
   `error` (code/title/detail) are implemented. `send_dtmf` exists
   (`POST /v2/calls/{id}/actions/send_dtmf`, `{"digits":…}`), so the
   `ErrDTMFUnsupported` fallback was not needed.
5. **`openai.base_url` allows a path** (§9.1/§9.2). The documented default is
   `https://api.openai.com/v1`, which is not an origin; validation requires absolute https
   with no query/userinfo/fragment but permits a path (the adapter appends
   `/chat/completions`). The "origin" wording in §9.2 is superseded by the concrete default.
6. **Transcript `outcome=` is empty at capture time** (§7.2). `recorder.New` has no
   `OrderStore` per §4.1, and the runner persists the terminal outcome after the call; the
   header therefore records the outcome known at wrap time (normally none). The field is
   retained for future enrichment; documented in `docs/recording.md`.
7. **Unexpected inbound Telnyx calls are hung up** (not in §5). The adapter dials outbound
   calls only; a `call.initiated` with `direction=incoming` for a known session is rejected
   with a hangup rather than driven through the runner.
8. **`buildRecorder` is always wired** (§4.2/§10 step 8). Returning nil when the global
   flags are off would make the §7.4 per-order override (`Recording:&true` over a global
   off) impossible. With flags off `Wrap` returns the identical session and starts no
   goroutine, so the default-off contract is unchanged.
9. **The swapability integration test uses fake STT/TTS** (§11). The test's purpose is the
   telephony + dialogue swap: it runs the real Telnyx and OpenAI adapters through the real
   runner/agent with deterministic in-repo STT/TTS fakes rather than the Mistral adapters.
   The full Mistral chain remains covered by the unchanged M0/M1 integration suites.
