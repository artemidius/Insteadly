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
         config, recorder, stub, twilio, telnyx,
         notify, agent…)   openai, notify/*, …)
```

- `internal/core/domain` imports **stdlib only** (plus nothing else).
- `internal/core/ports` imports `domain` only.
- `internal/core/{events,scheduler}` import `domain`/`ports` and stdlib only;
  `internal/core/auth` imports only `core/{ports,domain}` + stdlib (crypto/rand,
  crypto/sha256, encoding/base64); bcrypt lives in `internal/adapters/password/bcrypt`.
- `internal/core/recorder` (C7) imports only `domain`/`ports` + stdlib; it never imports
  `config` or an adapter. `internal/core/notify` (C7) imports `domain`/`ports`/`events` +
  stdlib. Both are covered by `make check-deps`.
- `internal/core/config` stays stdlib-only (it never imports pgx, stripe-go, or bcrypt).
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

C5 adds two **additive** order fields and two methods:

- `next_attempt_at` (scheduled due time; nil = due now) and `attempt_history`
  (append-only `AttemptRecord`s of prior finished attempts).
- `Schedule(next)` is the only `paid → scheduled` path besides `Requeue(next)`, which
  re-opens a **finished** order with a retryable outcome (`callback_needed` or
  `failed/{busy,no_answer,ivr_dead_end,error}`) while appending the finished attempt to
  history. `booked`/`aborted`/`rejected`/`other` are never requeued. The spine transition
  table is unchanged. `callback_needed` remains a terminal outcome, not a status.

C6 adds an ownership/payment dimension to the same spine, additively:

- `Order.UserID` (`domain.UserID`, empty when auth is off or a machine created the order),
  `Order.PaidAt`, and `Order.PaymentReference` (the provider's opaque payment id, unique per
  paid order).
- `Order.MarkPaid(reference, at)` is the **only** C6 `created → paid` writer. It is
  idempotent: an order already past `created` is returned unchanged, so a redelivered
  webhook is a no-op. It uses `Transition(OrderPaid)`, so the spine table is unchanged.
- `domain.User`/`domain.UserID`/`domain.Session`. `User.PasswordHash` and
  `Session.TokenHash` are `json:"-"`: neither a password hash nor a raw session token is
  ever serialized. `NewUserID` is 32 lowercase hex.
- `domain.PaymentEvent` gains the additive `Reference` field (the Stripe checkout session
  id) so the webhook bridge can persist `payment_reference`.

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
| `SchedulerStore` | `ports/storage.go` | scheduler | list due orders, lease-claim (`ports.ErrNotEligible`), release; a claim never changes status |
| `Payments` | `ports/payments.go` | API/webhook (C6) | Checkout redirect + signed webhook; `stub` (dev) or `stripe` adapter |
| `AuthStore` / `PasswordHasher` | `ports/auth.go` | core auth service (C6) | users/sessions persistence + password hashing; local/Postgres + bcrypt adapters |
| `Notifier` | `ports/notify.go` | `core/notify` worker (C7) | terminal outcome → one notification; `log`/`webhook` adapters, `off` by default |

Sentinel errors shared by adapters: `ports.ErrNotFound`, `ports.ErrConflict`,
`ports.ErrNotImplemented`, `ports.ErrNotEligible`. Channel closure and backpressure rules
are written in the interface doc comments and must be honored by every adapter.

## Composition root

`internal/app.New` turns a validated `config.Config` into the `App` object graph:

- `Storage`: `local`, or `postgres` (orders in Postgres, **blobs still local** via a
  composite `ports.Storage`). The Postgres adapter runs embedded migrations at startup.
- `Telephony`: `stub`, `twilio` in `voice-api` mode (C1), or `telnyx` (C7). Twilio
  `sip-trunk` still returns `ports.ErrNotImplemented`.
- `STT`/`TTS`: `stub` or `mistral` (C2). `Dialogue`: `stub`, `mistral` (C2), or `openai`
  (C7).
- `Payments` is `stub` (default) or `stripe`; `Auth` is one `core/auth.Service` built from
  the storage `AuthStore` + the bcrypt `PasswordHasher`.
- `Notify` (C7) is `nil` when `notify.provider=off` (the default); otherwise it is a
  `log`/`webhook` `ports.Notifier` plus one `core/notify.Worker` subscribed to the hub.
- `Recorder` (C7) is always injected into the runner: with every recording flag off it
  returns the dialed session unchanged, and a per-order override can still enable capture.
- `Agent` is one stateless `internal/core/agent.Agent` around the selected STT/TTS/
  Dialogue/Orders ports; `Runner` is one `internal/core/runner.Runner` around telephony,
  orders, and the agent.
- `Events` is one `internal/core/events.Hub`; the runner and scheduler publish to it and
  the HTTP SSE handler subscribes. `Scheduler` is one `internal/core/scheduler.Scheduler`
  around the scheduler store, the order store, and the runner.
- `App.Start(ctx)` starts the notify worker (when configured), then the scheduler
  (idempotent, no-op when disabled).
  `App.Close(ctx)` stops the scheduler, drains the runner, stops the notify worker (so it
  can still deliver a final finished event), then closes the hub — in that order.
  `App.CloseStorage()` releases the Postgres pool **last**, after HTTP shutdown.
- `App.Webhooks` carries the telephony adapter's HTTP surface (nil for stub);
  `internal/httpapi` mounts it, so the JSON API package never imports an adapter.

Adding a provider = one adapter package + one `case` in `internal/app/providers.go`.
No core changes, by design.

## Local storage adapter

```
<dir>/
├── orders/<32-hex-id>.json          # 0600, JSON indent 2
├── blobs/<32-hex-id|_system>/<kind>/<name>
└── auth/users.json                  # 0600, email → {id, email, password_hash, created_at}
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
- The local store also implements `ports.SchedulerStore`: `ListEligible` selects paid or
  due scheduled orders, `Claim` records an **in-memory** lease (single-process only), and
  `Release` clears it. `next_attempt_at`/`attempt_history` persist through the normal JSON
  file.
- It also implements `ports.AuthStore` (C6). Users persist in `auth/users.json` (written
  atomically, 0600); the file uses an adapter-local record because
  `domain.User.PasswordHash` is intentionally not a JSON field. Sessions are **in memory**,
  keyed by `sha256(raw token)`; a restart logs everyone out (fine for dev).

## HTTP layer

`internal/httpapi` builds a stdlib `http.ServeMux` with Go 1.22 method patterns:

| Route | Purpose |
|---|---|
| `GET /healthz` | liveness + version + uptime + provider names (never secrets) |
| `GET /api/v1/orders` | list, newest first, `[]` when empty |
| `POST /api/v1/orders` | create (server-assigned ID, strict JSON, 1 MiB cap) |
| `GET /api/v1/orders/{id}` | fetch one |
| `POST /api/v1/orders/{id}/call` | make an order eligible to dial now (`202`); API-key-gated when configured |
| `GET /api/v1/orders/{id}/events` | SSE: one order snapshot, then live status events |
| `POST /api/v1/auth/register` | create account + session cookie (`auth.mode=cookie`; `501` when off) |
| `POST /api/v1/auth/login` | verify credentials + session cookie |
| `POST /api/v1/auth/logout` | revoke session + clear cookie (`204`) |
| `GET /api/v1/auth/me` | bootstrap the SPA session |
| `POST /api/v1/orders/{id}/checkout` | payment redirect URL (owner-scoped) |
| `GET /api/v1/orders/{id}/checkout/simulate` | stub payments: mark paid + `303` dashboard |
| `POST /webhooks/payments/stripe` | signed Stripe webhook (mounted only in stripe mode) |
| `GET /` | embedded SPA + client-side fallback (GET-only; `/api` and `/webhooks` misses are 404) |

Middleware order is `recoverer → requestID → logger`; user routes additionally pass through
`sessionAuth` when `auth.mode=cookie`. `sessionAuth` stores the resolved user in the request
context; handlers enforce ownership and answer `404` for non-owned orders. In
`auth.mode=off` it is a passthrough, preserving the C5 API. Every response carries
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

## Orchestration (C5)

### Event hub

`internal/core/events.Hub` is a bounded fan-out (64 events per subscriber by default).
`Publish` is non-blocking: a slow subscriber's channel is closed and counted as dropped
rather than stalling a call. The runner publishes after every persisted status change
(dialing/ringing/in_call/finished); the scheduler publishes `paid → scheduled`, requeues,
and exhausted decisions. The HTTP layer only subscribes. A closed subscription is the
client-visible signal to reconnect and re-snapshot.

### Scheduler

`internal/core/scheduler` is one in-process loop (the C7 seam is River):

1. On each tick, if the configured opening windows allow `now`, list up to the free
   concurrency slots from `SchedulerStore`.
2. Lease-claim each candidate (never changing status). A paid order is moved to
   `scheduled(next_attempt_at = now)` via the domain method; a scheduled one goes straight
   to `Runner.Run`.
3. `Runner.Run` remains the **only** `scheduled → dialing` transition and `Attempts++`.
4. After the attempt, classify the persisted outcome: `booked`/`aborted`/`rejected`/`other`
   are terminal; `callback_needed` requeues at `callback_at`; `busy` (15m),
   `no_answer`/`ivr_dead_end` (30m), and `error` (5m) requeue with a linear
   attempt multiplier, advanced to the next opening window. At `max_attempts` a retryable
   outcome becomes terminal `failed/attempts_exhausted`.
5. Every lease is released when the attempt returns; lease expiry is the crash-safety net.

Clocks are injectable (`scheduler.Clock`, `scheduler.FakeClock`), windows are a config JSON
list (Mon–Fri 08:00–18:00 Europe/Vienna by default) and quiet hours are simply the
complement of the windows. The loop is bounded by `max_concurrent`; `Close` stops issuing,
cancels in-flight attempts, and waits for them. `Scheduler.RunOnce`/`Wait` exist so tests
drive scans deterministically under a fake clock.

### Postgres adapter

`internal/adapters/storage/postgres` implements `ports.OrderStore` and
`ports.SchedulerStore` with pgx v5 only (no ORM):

- `orders` table with JSONB `clinic`/`patient`/`outcome`/`recording`/`attempt_history`,
  timestamptz columns, and adapter-local `lease_owner`/`lease_until`.
- Embedded migrations (`migrations/0001_orders.sql`) applied in one transaction under a
  `schema_migrations` version table and an advisory lock, so startup is idempotent and
  concurrent instances do not race.
- `Update` runs `SELECT … FOR UPDATE`, the callback, and the write in one transaction —
  the runner's duplicate-dial fence stays intact cross-process.
- `ListEligible` uses `FOR UPDATE SKIP LOCKED`; `Claim` locks the row, re-checks
  status/lease/due-ness, and updates only the lease columns.
- Postgres mode does **not** expose a blob store; the composition root composes it with
  the local blob adapter.

### Lifecycle

`cmd/insteadly` starts the scheduler before serving HTTP. On SIGTERM/SIGINT it runs:
scheduler → runner → event hub → `http.Server.Shutdown` → Postgres pool close. The pool is
last so a persisting runner/scheduler never touches a closed pool; HTTP drains after the
runner because an active call needs the telephony HTTP/WebSocket surface while it cancels
and hangs up.

## Web app, auth, and payments (C6)

### Auth

`internal/core/auth.Service` is provider-neutral: register/login/logout and cookie-session
validation, with an injectable clock and TTL. It stores only `sha256(hex)` of a 256-bit
random token and relies on `ports.AuthStore` for persistence and `ports.PasswordHasher` for
hashing; bcrypt is the C6 adapter (`internal/adapters/password/bcrypt`, cost 12 by default,
validated 4..31). Login returns the same `ErrBadCredentials` for an unknown email and a
wrong password (no existence oracle), and a malformed hash verifies as `false`.

Cookies are `HttpOnly`, `SameSite=Lax`, `Path=/`, with `Max-Age` equal to the session TTL;
`Secure` is derived from an `https://` `INSTEADLY_HTTP_PUBLIC_URL`. A dedicated CSRF token,
password reset, email verification, and rate limiting are documented C7 hardening; JSON
bodies and SameSite raise the CSRF bar for C6.

`auth.mode=off` (the default) keeps the C5 no-env behavior: unauthenticated orders with
`user_id` empty, and auth/checkout routes answering `501 not_implemented`.
`auth.mode=cookie` enables the full web flow and scopes order list/get/events/checkout to
the session user. The C5 `POST /orders/{id}/call` route and `INSTEADLY_API_KEY` are
unchanged (dev/ops only).

### Payments

Two adapters implement `ports.Payments`:

- `internal/adapters/payments/stub` (default): `CreateCheckout` returns the authenticated
  simulate URL. The simulate endpoint performs the same `Order.MarkPaid` update the Stripe
  webhook does, so the scheduler path is identical without Stripe credentials.
- `internal/adapters/payments/stripe`: Checkout Sessions via
  `stripe-go/v87` (`stripe.Client`), and `checkout.session.completed` verification through
  `webhook.ConstructEventWithOptions` (HMAC-SHA256, API-version mismatch tolerated). The
  session is correlated by `metadata.order_id` / `client_reference_id`; the webhook bridge
  marks the order paid with the session id as `payment_reference`.

Durable replay safety comes from the domain (`MarkPaid` is a no-op outside `created`) plus a
partial unique index on `orders.payment_reference`. The webhook route exists only in stripe
mode; a bad signature is a `400` (not retryable), and an unknown order is acknowledged with
`200`. Stripe keys/secret are trimmed and non-empty-checked by config, parsed only inside
the adapter, redacted in `Summary`, and never logged.

### SPA serving and build tags

`web/` is a React 19 + TypeScript + Vite app. It consumes the snake_case JSON API verbatim
(no DTO layer), bootstraps the session with `/auth/me`, and subscribes to
`/orders/{id}/events` with `EventSource`. There is no client router or state library; one
`AuthProvider` context and a small `useState` view switch are enough.

`web/embed.go` (`//go:build webembed`) embeds `all:dist`; `web/noembed.go` (`!webembed`)
returns a placeholder, and `web/serve.go` always compiles and serves either the embedded SPA
with an `index.html` fallback or the placeholder. This keeps `go build ./...` and
`make verify` green without Node. `make build` runs the npm build first and compiles with
`-tags webembed`; `make build-go` is the Node-free backend build. Release artifacts are
built with `webembed`.

### Postgres schema

Migration `0002_users.sql` adds `users` (unique normalized email, bcrypt hash),
`sessions` (token hash primary key, `ON DELETE CASCADE` to users, expiry index), and the
`orders.user_id`/`paid_at`/`payment_reference` columns with an owner index and the partial
unique payment-reference index. `orders.user_id` references `users(id)`; pre-C6 rows keep
`NULL`. The migration runner applies `0001` then `0002` idempotently under the same advisory
lock.

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

## Telnyx telephony adapter (C7)

`internal/adapters/telephony/telnyx` is the second `ports.Telephony`/`ports.CallSession`
adapter. It uses Telnyx Call Control REST + call-control webhooks and is the **server** for
Telnyx's bidirectional Media Streams WebSocket. Provider concepts (`call_control_id`,
`connection_id`, `stream_id`, µ-law, Ed25519 signatures) never leave the package.

- Dial `POST /v2/calls` → `call.initiated`/`call.answered`/`call.hangup` webhooks; on
  answer, `POST /v2/calls/{id}/actions/streaming_start` with `stream_track=both_tracks`,
  `stream_bidirectional_mode=rtp`, `stream_bidirectional_codec=PCMU`.
- Webhooks are Ed25519-signed (`telnyx-signature-ed25519` + `telnyx-timestamp`, message
  `{ts}|{body}`); the media-stream handshake is signed over `{ts}|{stream_url}`. With the
  optional `INSTEADLY_TELNYX_WEBHOOK_PUBLIC_KEY`, signed requests must verify (fail closed);
  unsigned callbacks rely on the unguessable per-call path plus a `call_control_id` (and,
  when present, `connection_id`) match.
- Media stream: `start`/`media`(µ-law 8 kHz)/`dtmf`/`mark`(echo)/`stop`/`error` inbound;
  `media`/`mark`/`clear` outbound. The adapter has a self-contained G.711 + 8↔16 kHz codec
  (a copy, not an import, of the Twilio conversion).
- Outbound DTMF uses `POST /v2/calls/{id}/actions/send_dtmf`; hangup treats `90018`
  (already ended) as success.

Mounted by `httpapi` when `telephony.provider=telnyx`:

| Route | Purpose |
|---|---|
| `POST /webhooks/telephony/telnyx/webhook/{callID}` | call-control events |
| `GET /webhooks/telephony/telnyx/stream/{callID}` | bidirectional media-stream WebSocket |

All tests use in-repo fake REST/WebSocket servers — no account or live call. See
[telnyx.md](telnyx.md) for the verified protocol, signatures, and plan deviations.

## OpenAI dialogue adapter (C7)

`internal/adapters/dialogue/openai` is the second `ports.Dialogue` adapter and the C7
"alternative AI provider". It is structurally parallel to `dialogue/mistral`: Chat
Completions SSE over stdlib `net/http` + `bufio`, local history, fragmented tool-call
assembly, and the reserved `finish_call` outcome with strict `domain.Outcome` validation.
The OpenAI-specific differences are the base URL (`https://api.openai.com/v1`), the model
(`gpt-4o-mini` default), and the `max_completion_tokens` request field (a test pins it).

STT/TTS stay on Mistral in C7; one alternative AI port is enough to prove the swap promise.
All tests use a single `httptest` SSE server — no key, no network. See
[openai.md](openai.md).

## Recording and notifications (C7)

The recording seam is one optional `runner.Config.Recorder` field. `internal/core/recorder`
resolves `recording` as the master gate over the per-order `domain.RecordingFlags`, tees the
dialed `ports.CallSession` (forwarding every method, including optional `StreamReady`), and
on finalize persists two mono WAVs and/or a batch transcript through `ports.BlobStore`.
Flags off ⇒ the original session, no goroutine, zero bytes. `internal/core/notify` is a core
worker over the existing `events.Hub`: `finished` + non-nil `Outcome`, deduped by
`(OrderID, Attempts)`, resolved recipient, best-effort `Notifier.Send`. See
[recording.md](recording.md) and [notifications.md](notifications.md).

## Extension points

| Chunk | Change |
|---|---|
| C1 | ✅ (code) `internal/adapters/telephony/twilio` + `buildTelephony` case + `App.Webhooks`; live audio awaits a Twilio account upgrade |
| C2 | ✅ (code) `internal/adapters/{stt,tts,dialogue}/mistral` + three `build*` cases; live Mistral smoke passed |
| C3 | ✅ code complete (real-Mistral no-phone gate passed) `internal/core/agent` + `internal/core/runner`; no new API route |
| C4 | 🔵 code complete pending the live no-phone gate: streaming VAD/turn-taking, barge-in, Twilio clear/mark, Mistral interrupt/control, TTS abort, IVR, hold, recovery; no new module/env/API surface |
| C5 | 🔵 code complete pending conductor verification: `internal/adapters/storage/postgres`, `internal/core/{scheduler,events}`, place-call + SSE in `httpapi`, Postgres-gated testcontainers suites |
| C6 | 🔵 code complete pending conductor verification: users/sessions + cookie auth, Stripe Checkout + signed webhook + stub, ownership scoping, React+TS SPA embedded behind `webembed` |
| C7 | 🔵 code complete pending conductor verification: `telephony/telnyx`, `dialogue/openai`, `core/recorder` + `runner.Config.Recorder`, `core/notify` + `notify/{log,webhook}`, config/env wiring; **no port/domain change**; no new module dep. Still later: durable outbox, CSRF/reset/rate limiting, Postgres blobs, deployment (Q5) |
