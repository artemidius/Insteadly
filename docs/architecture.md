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

Other domain shapes: `CallEvent`/`EndReason` (normalized telephony), `Transcript`,
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
| `Telephony` / `CallSession` | `ports/telephony.go` | call runner (C3) | normalized events, duplex canonical audio, DTMF, idempotent hangup |
| `SpeechToText` / `TranscriptionStream` | `ports/speech.go` | agent engine (C3) | streaming + batch; final flush on `Close` |
| `TextToSpeech` / `SpeechStream` | `ports/speech.go` | agent engine (C3) | feed text, canonical audio out; not 1:1 |
| `Dialogue` / `DialogueSession` / `AssistantTurn` | `ports/dialogue.go` | agent engine (C3) | text + tools in, streamed text + tool calls + outcome out |
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
- `STT`, `TTS`, `Dialogue`: `stub`; `mistral` returns `ports.ErrNotImplemented` until C2.
- `Payments` and `Notify` remain `nil` until C6/C7.
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
IDs. This is a C1 stopgap; C4 may replace it with a more robust IVR flow.

## Extension points

| Chunk | Change |
|---|---|
| C1 | ✅ (code) `internal/adapters/telephony/twilio` + `buildTelephony` case + `App.Webhooks`; live audio awaits a Twilio account upgrade |
| C2 | `internal/adapters/{stt,tts,dialogue}/mistral` + three `build*` cases |
| C3 | `internal/core/agent` (loop, VAD) + `internal/core/runner`; consumes existing ports |
| C5 | Postgres storage adapter, scheduler, SSE endpoint in `httpapi` |
| C6/C7 | Payments and Notifier adapters; fill the `nil` fields in `app.App` |
