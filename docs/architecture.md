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
- Adapters import `ports` + `domain` only; they never import each other or `app`.
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

- `Storage`: `local` (only provider in C0).
- `Telephony`, `STT`, `TTS`, `Dialogue`: `stub` in C0; `twilio`/`mistral` return
  `ports.ErrNotImplemented` until C1/C2.
- `Payments` and `Notify` remain `nil` until C6/C7.

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

## Extension points

| Chunk | Change |
|---|---|
| C1 | `internal/adapters/telephony/twilio` + one `buildTelephony` case |
| C2 | `internal/adapters/{stt,tts,dialogue}/mistral` + three `build*` cases |
| C3 | `internal/core/agent` (loop, VAD) + `internal/core/runner`; consumes existing ports |
| C5 | Postgres storage adapter, scheduler, SSE endpoint in `httpapi` |
| C6/C7 | Payments and Notifier adapters; fill the `nil` fields in `app.App` |
