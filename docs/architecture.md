# Architecture

Insteadly is a **hexagon**: a provider-agnostic core, adapter packages for every
external capability, and a single composition root. The same rules apply as long as the
project exists — see `make check-deps` and [../DESIGN.md](../DESIGN.md).

## Layers and dependency rule

```
                 cmd/insteadly
                      │
        ┌─────────────┴─────────────┐
        ▼                           ▼
  internal/httpapi            internal/core/config
        │                           │
        └──────────────┬────────────┘
                       ▼
                 internal/app  ◄── the only package importing both
                  │        │        core and adapters
                  ▼        ▼
        internal/core/*   internal/adapters/*
        (domain, ports,   (storage/local,
         config)           stub, twilio… in C1+)
```

- `internal/core/domain` imports **stdlib only** (plus nothing else).
- `internal/core/ports` imports `domain` only.
- Adapters import `ports` + `domain` only (plus vetted third-party libraries used entirely
  inside the adapter); they never import each other or `app`.
- `internal/httpapi` talks to ports through `*app.App`; it contains no business rules.
- Nothing outside `internal/core` implements core logic; nothing outside
  `internal/adapters` knows a provider SDK.

## Domain model

`Order` is the spine. Statuses move forward only:

```
created → paid → scheduled → dialing → ringing → in_call → finished
```

`finished` is reachable **only** through `Finish(Outcome)`, which validates the outcome:

| Outcome | Requires |
|---|---|
| `booked` | `booked_slot.date_time` (and doctor/practice as available) |
| `callback_needed` | `callback_at` |
| `failed` | `fail_reason` (busy, no_answer, rejected, ivr_dead_end, …) |
| `aborted` | nothing else |

`Transition` and `Finish` stamp `updated_at`. Retry-from-`failed` is a scheduler
concern (C5) and is deliberately absent from the state machine. Corrupt or illegal
transitions return `domain.ErrInvalidTransition`; bad outcomes return
`domain.ErrInvalidOutcome`.

Other domain shapes: `CallEvent`/`EndReason` (normalized telephony, including
`EventPlaybackMark` with an opaque `PlaybackID`), `Transcript`,
`AudioChunk` + canonical audio constants, `BlobKey`, `Tool`, `Notification`,
`PaymentEvent`.

### Canonical audio

Every audio stream inside the core is **PCM16 little-endian, mono, 16 kHz**
(`domain.SampleRate` etc.). 20 ms frames are `FrameBytes = 640`. All codec, container,
and sample-rate conversions happen inside adapters — Twilio's 8 kHz µ-law never crosses
the port boundary.

## Ports

| Port | File | Consumer | Notes |
|---|---|---|---|
| `Telephony` / `CallSession` | `ports/telephony.go` | call runner (C3/C4) | normalized events incl. playback marks, duplex canonical audio, DTMF, ordered `ClearAudio`/`MarkPlayback`, idempotent hangup |
| `SpeechToText` / `TranscriptionStream` | `ports/speech.go` | conversation engine (C4) | one stream per VAD utterance; final flush on `Close` |
| `TextToSpeech` / `SpeechStream` | `ports/speech.go` | conversation engine (C4) | feed text, canonical audio out, non-flushing `Abort` for barge-in; not 1:1 |
| `Dialogue` / `DialogueSession` / `AssistantTurn` | `ports/dialogue.go` | conversation engine (C4) | text + tools in, streamed text + tool calls + outcome out; `Interrupt`/`SendControl` reconciliation |
| `Storage` → `OrderStore`, `BlobStore` | `ports/storage.go` | API, runner, scheduler | orders + blobs; no provider types |
| `Payments` | `ports/payments.go` | API/webhook (C6) | declared, no adapter yet |
| `Notifier` | `ports/notify.go` | runner (C7) | declared, no adapter yet |

Sentinel errors shared by adapters: `ports.ErrNotFound`, `ports.ErrConflict`,
`ports.ErrNotImplemented`. Channel closure and backpressure rules are written in the
interface doc comments and must be honored by every adapter.

## Composition root

`internal/app.New` turns a validated `config.Config` into the `App` object graph:

- `Storage`: `local`.
- `Telephony`: `stub`, or `twilio` in `voice-api` mode (C1). `sip-trunk` still returns
  `ports.ErrNotImplemented` (C7).
- `STT`, `TTS`, `Dialogue`: `stub`, or `mistral` (C2).
- `Payments` and `Notify` remain `nil` until C6/C7.
- `Agent` is one stateless `internal/core/agent.Agent` around the selected STT/TTS/
  Dialogue/Orders ports; `Runner` is one `internal/core/runner.Runner` around telephony,
  orders, and the agent. `App.Close` drains active calls through the runner.
- `App.Webhooks` carries the telephony adapter's HTTP surface (nil for stub);
  `internal/httpapi` mounts it, so the JSON API package never imports an adapter.

Adding a provider = one adapter package + one `case` in `internal/app/providers.go`.
No core changes, by design.

## Local storage adapter

```
<dir>/
├── orders/<32-hex-id>.json          # 0600, JSON indent 2
└── blobs/<32-hex-id|_system>/<kind>/<name>
```

- `_system` holds blobs without an order ID; it cannot collide with a 32-hex ID.
- **Path safety:** IDs must match `^[0-9a-f]{32}$`; blob kinds are allowlisted to
  `recording`/`transcript`; names are plain filenames (no separators, `.`/`..`, NUL).
  Invalid IDs on read paths are reported as `ErrNotFound` (they cannot exist).
- **Atomic writes:** same-directory temp file → chmod `0600` → fsync → rename. A crash
  leaves at worst a temp file, never a torn record.
- `Create` assigns IDs/timestamps and forces status `created`. `List` is newest-first
  and fails loudly on a corrupt file. `Update` runs the callback under the store lock,
  so callbacks must be fast, I/O-free, and must not re-enter the store.
- One `sync.RWMutex` for the whole store: fine at prototype scale; per-key sharding is
  a later optimization.

## HTTP layer

`internal/httpapi` builds a stdlib `http.ServeMux` with Go 1.22 method patterns:

| Route | Purpose |
|---|---|
| `GET /healthz` | liveness + version + uptime + provider names (never secrets) |
| `GET /api/v1/orders` | list, newest first, `[]` when empty |
| `POST /api/v1/orders` | create (server-assigned ID, strict JSON, 1 MiB cap) |
| `GET /api/v1/orders/{id}` | fetch one |

Middleware order is `recoverer → requestID → logger`. Every response carries
`X-Request-Id` (inbound value honored). Errors use one envelope:
`{"error":{"code":"<snake_case>","message":"<human>"}}`, mapping
`ErrNotFound → 404`, `ErrConflict → 409`, oversized body → 413, decode/validation →
400, everything else → 500 (detail logged, never returned).

`config.Load` validates enums/required fields before adapters are built; the HTTP layer
adds request-shape validation (required clinic/patient fields, birth-date format,
preferred-slot ordering) and reports all violations in one 400.

## Twilio telephony adapter (C1)

`internal/adapters/telephony/twilio` implements `ports.Telephony` + `ports.CallSession`
using Twilio Programmable Voice and bidirectional Media Streams. Provider concepts
(`CallSid`, µ-law, TwiML, signatures) never leave the package.

### Outbound call flow

1. `Dial` validates the clinic number (E.164), creates a `domain.CallID`, registers a
   session, and `POST`s to the Twilio Calls resource with:
   - `Url` → our voice webhook;
   - `StatusCallback` → our status webhook (`initiated`, `ringing`, `answered`,
     `completed`);
   - `Timeout`, `From`, `To`.
2. Twilio status callbacks are normalized to `dialing → ringing → answered`; callbacks may
   arrive out of order, so progress is monotonic and gaps are synthesized.
3. On answer, Twilio fetches `<Response><Connect><Stream url="wss://…">` and opens a
   WebSocket. The adapter validates `start` (AccountSID, CallSid, the `call_id` custom
   parameter, µ-law/8000/1) before accepting media.
4. Inbound µ-law 20 ms frames are decoded, upsampled 8→16 kHz, and delivered as 640-byte
   canonical chunks. Outbound canonical chunks are downsampled with an anti-alias filter,
   µ-law encoded, and sent as 160-byte media frames.
5. Terminal callbacks (`completed`, `busy`, `no-answer`, `canceled`, `failed`) produce
   exactly one `EventEnded` with a normalized `domain.EndReason`; the event and audio
   channels are then closed. Finished sessions stay addressable for 5 minutes so late
   callbacks get a clean response, then are removed.

### HTTP surface

Mounted by `httpapi` only when `telephony.provider=twilio`:

| Route | Purpose |
|---|---|
| `POST /webhooks/telephony/twilio/voice/{callID}` | TwiML: connect the call to a Media Stream (or hang up a finished one) |
| `POST /webhooks/telephony/twilio/status/{callID}` | call progress callbacks |
| `POST /webhooks/telephony/twilio/stream-status/{callID}` | stream started/stopped/error (diagnostics) |
| `GET /webhooks/telephony/twilio/stream/{callID}` | bidirectional Media Streams WebSocket |

Status callbacks require a valid `X-Twilio-Signature` (HMAC-SHA1 over the public URL and
all form parameters). Twilio does **not** sign the TwiML `Url` fetch (verified live), so
that route is guarded by the unguessable call ID plus matching `AccountSid`/`CallSid` form
fields; if a signature is present it must still validate. Stream status callbacks are
diagnostic and accept a signature when Twilio sends one. The WebSocket handshake is
unsigned and is validated by the call ID plus the `start` payload. The public base URL
(`INSTEADLY_HTTP_PUBLIC_URL`) must exactly match the URL configured at Twilio, otherwise
status signatures fail with 403.

### Trial accounts

Twilio strips the `<Stream>` verb from custom TwiML on trial accounts and replaces it with
a "not available on trial accounts" message. Dialing, status callbacks, and TwiML fetching
work on trial; live Media Streams audio requires an upgraded account.

### DTMF

Inbound digits arrive as `dtmf` WebSocket messages and become `EventDTMF`. Twilio's
Media Streams socket cannot send DTMF, so `SendDTMF` updates the call with TwiML that plays
`<Play digits="…">` and reconnects the stream; the session is reconnect-safe across stream
IDs. This remains a C1 stopgap: C4's core IVR policy presses one evidence-backed digit at a
time, but the real outbound-DTMF path is still unproven on the Twilio trial.

### Playback clear and marks (C4)

C4 adds the Media Streams outbound `{event:"clear",streamSid}` and
`{event:"mark",streamSid,mark:{name}}` messages. One writer goroutine owns media/mark/clear
order; `ClearAudio` advances a playback epoch, purges queued actions, resets residual encoder
bytes, and sends `clear`, so old audio cannot race past it. Inbound `mark` becomes exactly
one `EventPlaybackMark` with the opaque core ID; late marks from a cleared generation are
valid races ignored by ID in the core. With no attached stream, clearing local playback
succeeds; after call end the established safe-end semantics apply. µ-law conversion, inbound
DTMF, `StreamReady`, reconnect, and `stop`-is-not-call-end are unchanged.

## Call runner and conversation engine (C4)

`internal/core/runner` owns one order attempt: atomic claim
(`scheduled → dialing`, `Attempts++`), one `Dial` on a call-lifetime context, event-driven
status transitions, the optional C1 `StreamReady()` wait, agent handoff, exactly one
`Hangup`, and exactly one `Finish(outcome)`. A valid dialogue outcome always wins over the
local Hangup reason (`EndAborted`); otherwise a fixed fallback maps the terminal
`EndReason`/provider error to `aborted` or `failed/{busy,no_answer,rejected,ivr_dead_end,
error,other}`. Post-answer `EventDTMF`/`EventPlaybackMark` are relayed on a separate
capacity-64 control channel; overflow closes it and fails the attempt rather than silently
losing a digit or mark — even if the agent raced to a valid outcome, the runner checks the
relay error and refuses to persist it. The terminal `EventEnded` signal stays capacity-one and independent.
`Runner.Run` is synchronous; `Runner.Close` rejects new runs, cancels and waits for active
calls, and `cmd/insteadly` drains it before `http.Server.Shutdown`.

`internal/core/agent` owns the streaming conversation engine (C4): continuous 20 ms framing
and stdlib DSP, adaptive noise/VAD with 60 ms onset and 700 ms hangover, one STT stream per
utterance with live pre-roll+frame feeding, full-duplex barge-in with bounded TTS
`Abort`/`ClearAudio`, per-segment playback marks and conservative confirmed-text history,
deterministic evidence-gated IVR navigation, conservative 3 s hold tone/music detection, and
at most two structured-outcome recovery controls per call. C3's greeting, core-owned tools,
`ValidateOutcome`, booking invariants, and exactly-once runner finalization are preserved.
The agent's narrow `AudioSession` can read/send audio, send DTMF, clear queued audio, and
place marks; it still cannot consume telephony events or hang up. The agent stops/joins its
audio workers before closing the dialogue session; cleanup is 35 s to exceed C2's 30 s TTS
final drain, while interruption is bounded separately at 2 s.

Both packages import only `internal/core/{ports,domain}` (plus the agent from the runner);
`internal/app` is the only composer. There is deliberately **no** public place-call
endpoint in C4 — `Runner.Run` is the invocation surface until C5 owns authenticated,
idempotent scheduling. Exact state machine, DSP constants, barge/hold/IVR/recovery rules,
privacy constraints, and live-test recipes are in [agent.md](agent.md).

## Mistral adapters (C2)

`internal/adapters/{stt,tts,dialogue}/mistral` implement `ports.SpeechToText`,
`ports.TextToSpeech`, and `ports.Dialogue` against Mistral's raw HTTP/SSE/WebSocket APIs
(no Go SDK). Provider concepts (model names, base64 audio, SSE frames, 24 kHz float32 PCM)
never cross a port boundary.

- **STT** (`stt/mistral`): realtime `TranscriptionStream` over WebSocket (canonical PCM16
  16 kHz input → cumulative partials → one final on `input_audio.end`), plus batch
  `TranscribeFile` (raw PCM → in-memory WAV → multipart POST → `domain.Transcript`).
- **TTS** (`tts/mistral`): realtime `SpeechStream` (incremental text → float32 24 kHz audio
  → 15-tap anti-alias + 3:2 polyphase resampling → canonical PCM16 16 kHz, 640-byte frames)
  and an `offline` mode that POSTs `/v1/audio/speech` (`stream=true`) on `Close`. C4 adds a
  non-flushing `Abort` for barge-in: realtime cancels the WebSocket generation, offline
  cancels an in-flight request or prevents one before `Close`; normal `Close` still flushes.
- **Dialogue** (`dialogue/mistral`): Chat Completions SSE with local history, fragmented
  tool-call assembly, exact tool-result continuation, and the reserved `finish_call` outcome
  (strict `domain.Outcome` decode + validation). C4 adds `Interrupt(confirmedText)` for
  history rollback/reopen and `SendControl(instruction)` as a system-role control turn,
  retaining the failed/stalled candidate without inventing user speech.

See [mistral.md](mistral.md) for the exact protocols, config surface, tool/outcome contract,
and live-test recipe.

## Extension points

| Chunk | Change |
|---|---|
| C1 | ✅ (code) `internal/adapters/telephony/twilio` + `buildTelephony` case + `App.Webhooks`; live audio awaits a Twilio account upgrade |
| C2 | ✅ (code) `internal/adapters/{stt,tts,dialogue}/mistral` + three `build*` cases; live Mistral smoke passed |
| C3 | ✅ code complete (real-Mistral no-phone gate passed) `internal/core/agent` + `internal/core/runner`; no new API route |
| C4 | 🔵 code complete pending the live no-phone gate: streaming VAD/turn-taking, barge-in, Twilio clear/mark, Mistral interrupt/control, TTS abort, IVR, hold, recovery; no new module/env/API surface |
| C5 | Postgres storage adapter, scheduler, SSE endpoint in `httpapi` |
| C6/C7 | Payments and Notifier adapters; fill the `nil` fields in `app.App` |
