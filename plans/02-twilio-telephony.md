# Chunk C1 — Twilio telephony adapter (voice-api)

> **Status:** implemented 2026-11-17 (code complete). Live Media Streams audio is blocked by
> the Twilio **trial** account brand — see Amendments.
> **Source of truth:** [DESIGN.md](../DESIGN.md) — this plan derives from it and must not
> contradict it. If a conflict appears, stop and resolve it against DESIGN.md, not silently.
> **Progress register:** [PROGRESS.md](../PROGRESS.md) — update it at the end of the session
> that implements this chunk (see its "Update protocol").
> **Previous chunk:** [plans/01-foundation.md](01-foundation.md) (C0, ✅).

---

## 1. What this chunk is

C1 replaces the `twilio` branch in `internal/app/providers.go` (today: `ErrNotImplemented`)
with a real adapter behind **`ports.Telephony`**. It is the first provider that reaches the
outside world: REST dial, Twilio webhooks, and a bidirectional Media Streams WebSocket
carrying audio.

The point is not "Twilio support" — it is proving the **seam** (DESIGN.md §1.2): the core
keeps speaking only `domain.CallEvent`, `domain.AudioChunk` (PCM16 LE mono 16 kHz), and
`ports.CallSession`; Twilio concepts (`CallSid`, streams, µ-law, form-encoded webhooks) never
cross the port boundary.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | Twilio REST client: create call, update call (DTMF redirect), typed API errors | `internal/adapters/telephony/twilio/client.go` |
| 2 | `ports.Telephony` + `ports.CallSession` implementation, session registry | `.../twilio/adapter.go`, `.../session.go` |
| 3 | HTTP webhook surface: TwiML (voice), status callbacks, stream status | `.../twilio/webhook.go` |
| 4 | Media Streams WebSocket server (Twilio → us, us → Twilio) | `.../twilio/stream.go` |
| 5 | G.711 µ-law codec + 8 kHz ↔ 16 kHz conversion + 20 ms framing | `.../twilio/audio.go` |
| 6 | Webhook signature validation (HMAC-SHA1, `X-Twilio-Signature`) | `.../twilio/signature.go` |
| 7 | DTMF: inbound (`dtmf` message) and outbound (TwiML `<Play digits>` redirect) | `stream.go`, `session.go` |
| 8 | Config: public base URL, ring timeout, Twilio validation rules | `internal/core/config/` |
| 9 | Wiring: `app.Webhooks` + mounting in `httpapi` | `internal/app/`, `internal/httpapi/` |
| 10 | Fake-Twilio integration tests that need **no credentials and no network** | `.../twilio/*_test.go` |
| 11 | Operator docs + a build-tagged **live** call test | `docs/twilio.md`, `internal/app/live_test.go` |
| 12 | One third-party dependency (WebSocket): `github.com/coder/websocket` | `go.mod`, `go.sum`, CI |

### Out of scope (do NOT implement — later chunks)

- **Anything Mistral** (STT/TTS/Dialogue) → C2.
- **Agent engine / conversation loop / VAD / barge-in** → C3/C4. C1 only delivers the
  transport and the port behavior. No `clear`/mark-based barge-in yet (C4).
- **Call runner / scheduler / retries / order status transitions** → C3/C5. `Dial` is one
  attempt, called directly; no retry policy.
- **Inbound calls** to our Twilio number → not needed; outbound only.
- **SIP trunk mode** (`telephony.twilio.mode=sip-trunk`) → C7. The builder keeps returning
  `ErrNotImplemented` for it.
- **Twilio recordings** (`Record=true`, RecordingStatusCallback) and our own recording
  storage → C7 (recording flags end-to-end).
- **Postgres/River, Docker, deployment target** → later.
- **A public "place a call" API** → C3. C1 verifies with the build-tagged live test, not
  with a production endpoint.
- **Graceful draining of live calls on SIGTERM** → C3. Hijacked WebSocket connections are
  not tracked by `http.Server.Shutdown`; C1 accepts that a process exit drops them.

If any of the above creeps in, stop and note it in the PROGRESS session log instead.

---

## 2. Preconditions (verify first, in this order)

1. **Baseline is green.** Observed 2026-11-17:
   `go test -count=1 ./...` → all packages `ok`; `git status` clean (`.env` is git-ignored;
   `data/` absent). If the baseline is not green, fix that first.
2. **Twilio console state** (user-owned; never print values):
   - a Twilio phone number is owned and can be used as caller ID
     (`TWILIO_PHONE_NUMBER` is present in `.env`);
   - the destination (`TEST_PHONE`) is added as a **Verified Caller ID** — trial accounts
     may only call verified numbers;
   - the account has credit and the destination is permitted (Austria). Trial geo
     permissions can reject calls (Twilio error `21215`); the console is the fix.
   - **Cost warning:** the live test places one short real call (~10 s). Only run it with
     the user's explicit go-ahead.
3. **Public tunnel.** Twilio must reach our HTTP + WSS endpoints. `cloudflared` is **not
   installed** on this machine (checked 2026-11-17; `ngrok` and the Twilio CLI are also
   absent). Installing it is a user-approved machine change:
   ```bash
   brew install cloudflared          # ask before running
   cloudflared tunnel --url http://127.0.0.1:8080
   # note the printed https://<random>.trycloudflare.com URL
   ```
   Alternatives if the user prefers: an existing tunnel or a public host. The quick-tunnel
   URL changes on restart and must match `INSTEADLY_HTTP_PUBLIC_URL` exactly (scheme + host,
   no trailing slash), otherwise webhook signatures fail (403).
4. **Config names.** `.env` currently holds the **unprefixed** names
   `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `TWILIO_PHONE_NUMBER`, `TEST_PHONE`.
   C1 deliberately **does not** add a legacy fallback (the C0 invariant "non-`INSTEADLY_*`
   variables are ignored" stays true; see §5.3). The user must either add
   `INSTEADLY_TWILIO_*` lines to `.env` (agent must not edit `.env`) or export them in the
   shell. `TEST_PHONE` is only read by the live test (`os.Getenv`), never by the app.
5. **Dependency decision.** C1 adds **one** dependency: `github.com/coder/websocket`
   (latest v1.8.15, 2026-06-15; pure Go, **zero transitive dependencies**, MIT, maintained
   by Coder after nhooyr). Go's stdlib has no WebSocket server. Pin the version, commit
   `go.sum`, and update CI's `cache-dependency-path` to `go.sum`.
6. **Design check.** No conflict with DESIGN.md. One provider limitation is documented in
   §3.5: Twilio's bidirectional Media Streams cannot send DTMF over the socket, so outbound
   DTMF uses the call-update/TwiML path. That is an adapter implementation detail, not a
   port change.

---

## 3. How Twilio voice-api maps onto the port

### 3.1 End-to-end sequence

```
core (C3 later)                     twilio adapter                     Twilio
   │ Dial(ctx, order)                   │                               │
   ├───────────────────────────────────►│ validate E.164, CallID,       │
   │                                    │ register session              │
   │                                    ├── POST Calls.json ──────────►│  (From, To,
   │                                    │   Url, StatusCallback,        │   Timeout, …)
   │                                    │◄── 201 {sid,status} ──────────┤
   │◄── CallSession (immediately) ──────┤                               │
   │                                    │◄── status: initiated/ringing ─┤
   │◄── Events: dialing → ringing ──────┤                               │
   │                                    │        far end answers        │
   │                                    │◄── POST voice/<callID> ───────┤
   │                                    ├── 200 TwiML ────────────────►│
   │                                    │      <Connect><Stream …>      │
   │                                    │◄── WSS /stream/<callID> ──────┤
   │                                    │◄── connected, start ──────────┤
   │◄── Events: answered ───────────────┤                               │
   │◄── Inbound: 640 B PCM16/20 ms ─────┤◄── media (µ-law, base64) ─────┤
   │── SendAudio(640 B PCM16) ─────────►├── media (µ-law, base64) ─────►│
   │── SendDTMF("1") ──────────────────►├── POST Calls/<sid>.json ─────►│
   │                                    │      TwiML: Play digits +     │
   │                                    │      reconnect Stream         │
   │                                    │◄── status: completed ─────────┤
   │◄── Events: ended(completed) ───────┤                               │
   │◄── channels closed ────────────────┤                               │
```

`Dial` returns **after the REST create succeeds**, not after the call is answered. Progress
arrives through the status callback and the WebSocket. This matches the port contract: "It
returns a live CallSession without waiting for the far end to answer".

### 3.2 Public HTTP routes (adapter-owned, mounted by the composition root)

Base path: **`/webhooks/telephony/twilio`** (constant in the adapter, documented in
README/docs). Only mounted when `telephony.provider=twilio`.

| Method | Path | Auth | Response |
|---|---|---|---|
| `POST` | `/voice/{callID}` | `X-Twilio-Signature` | `200 text/xml` TwiML: `<Response><Connect><Stream …></Stream></Connect></Response>` |
| `POST` | `/status/{callID}` | `X-Twilio-Signature` | `200` empty body |
| `GET` | `/stream/{callID}` | WebSocket upgrade; path ID + `start` validation | `101` |
| `POST` | `/stream-status/{callID}` | `X-Twilio-Signature` | `200` empty body |

Rules:

- All routes go through the existing HTTP middleware (`recoverer → requestID → logger`).
  `statusRecorder` already implements `Unwrap()`, so `coder/websocket` can hijack the
  connection through it — this must be covered by an integration test.
- Body cap **64 KiB** on the form endpoints (Twilio payloads are small; unbounded form
  parsing is an easy DoS).
- Voice: unknown `callID` → `404`; ended `callID` → `200` Hangup TwiML. Stream status:
  unknown/ended → `200` + debug log (to stop retries).
- Status: **always `200` after a valid signature**, even for an unknown/ended call, because
  Twilio retries non-2xx. Unknown/ended is logged at debug and ignored (idempotent).
- TwiML responses: `Content-Type: text/xml; charset=utf-8`, `Cache-Control: no-store`.
- No query strings are used in the Twilio-facing URLs (`<Stream url>` does **not** support
  query parameters per Twilio docs); the call ID lives in the path, and `<Parameter>` carries
  it a second time for WS validation.

### 3.3 Event normalization and ordering

The core must never see `queued`/`initiated`/`in-progress` or a provider error code.

| Twilio signal | Domain event(s) emitted |
|---|---|
| `Dial` REST 2xx | `EventDialing` (immediately, before returning) |
| `CallStatus=queued` / `initiated` | `EventDialing` if not already emitted |
| `CallStatus=ringing` | `EventRinging` if not answered yet |
| `CallStatus=in-progress` | `EventAnswered` (once) |
| WS `start` | `EventAnswered` if the callback was missed |
| WS `dtmf` (`dtmf.digit`) | `EventDTMF` with `DTMF=<digit>` |
| `CallStatus=completed` | if not yet answered: `EventAnswered` first; then `EventEnded{EndCompleted}` |
| `CallStatus=busy` | `EventEnded{EndBusy}` |
| `CallStatus=no-answer` | `EventEnded{EndNoAnswer}` |
| `CallStatus=canceled` | `EventEnded{EndAborted}` |
| `CallStatus=failed` | `EventEnded{EndError}` |
| `Hangup()` (local) | `EventEnded{EndAborted}` (once), then best-effort REST end |
| WS `stop` | **no event** — a stream can stop and reconnect (DTMF redirect); the call ends via status callback or `Hangup` |
| unknown `CallStatus` | no event; `200`, debug log |

Ordering rules (Twilio explicitly warns status callbacks may arrive out of order):

1. Progress is monotonic: dialing(0) → ringing(1) → answered(2) → ended(3). A lower rank
   after a higher one is ignored (debug log). A jump synthesizes the missing progress
   event (e.g. `in-progress` before `ringing` emits `EventRinging` then `EventAnswered`).
2. `EventAnswered` and `EventEnded` each occur **at most once**.
3. `EventEnded` is always the **last** event; `Events()` and `Inbound()` are closed right
   after it is delivered.
4. DTMF events may occur any number of times after `answered`; they never change progress.
5. Every `CallEvent` carries `CallID` and `At=time.Now()`.

### 3.4 Audio pipeline (DESIGN.md §1.5)

Canonical audio: **PCM16 LE, mono, 16 kHz**; 20 ms = 320 samples = **640 bytes**
(`domain.FrameBytes`). Twilio: **µ-law, mono, 8 kHz**; a `media` payload is normally 160
bytes = 160 samples = 20 ms.

**Inbound (Twilio → core)** — per `media` message with `track == "inbound"`:

1. base64-decode `media.payload` (reject payloads > 8 KiB; a 20 ms chunk is ~216 base64
   chars).
2. G.711 µ-law → `int16` (table/algorithm in `audio.go`).
3. 8 kHz → 16 kHz by **linear interpolation** (160 → 320 samples).
4. Accumulate into whole 320-sample frames; emit `AudioChunk{Data: 640 bytes}` on
   `Inbound()`.
5. `Inbound()` is buffered (cap **100** ≈ 2 s). If the core is slow, **drop the oldest**
   chunk, count it, and log one warning with `inbound_dropped` when the call ends. Residual
   < 20 ms at stream end is discarded.

**Outbound (core → Twilio)** — `SendAudio`:

1. Enqueue `domain.AudioChunk` into a bounded queue (cap **100**). `SendAudio` blocks while
   the queue is full and returns when queued, when `ctx` is cancelled, or when the call has
   ended (`ErrSessionEnded`).
2. A single writer goroutine waits until a stream is attached, then for each whole
   320-sample block: 16 kHz → 8 kHz (anti-alias low-pass then decimate 2:1), µ-law encode
   (160 bytes), base64, one
   `{"event":"media","streamSid":"…","media":{"payload":"…"}}` frame.
3. Residual < 20 ms is dropped at end. **No pacing**: Twilio buffers outbound audio; TTS
   (C2) produces at roughly real-time. Barge-in / `clear` is C4.
4. If the socket write fails, log and detach; do not end the call (a reconnect may follow).
   The pending chunk is dropped; the core can keep sending.

**Resampling quality:** telephone intelligibility, not music. The 16→8 kHz path needs a
real anti-alias filter (a 9-tap Hamming-windowed sinc at ~3.4 kHz is enough; coefficients
live in `audio.go` with a comment). The 8→16 kHz path uses linear interpolation. Tests
assert: µ-law round-trip for all 256 codes, frame lengths, and that a 6 kHz tone is
attenuated relative to 1 kHz on the downsampling path.

### 3.5 DTMF

**Inbound:** Twilio sends `{"event":"dtmf","dtmf":{"track":"inbound","digit":"1"}}` on a
bidirectional stream. Map each digit to `domain.CallEvent{Kind: EventDTMF, DTMF: digit}`.
Digits are `0-9`, `*`, `#`, `A-D`; forward them verbatim.

**Outbound — important provider constraint:** Twilio's Media Streams WebSocket accepts only
`media`, `mark`, and `clear` messages from the server; there is **no outbound `dtmf`
message**. `SendDigits` on the Call resource only works at dial time (cannot react to an
IVR). The supported mid-call mechanism is to **update the call with TwiML**:

```xml
<Response>
  <Play digits="1"/>
  <Connect>
    <Stream url="wss://…/stream/<callID>">
      <Parameter name="call_id" value="<callID>"/>
    </Stream>
  </Connect>
</Response>
```

The update interrupts the current `<Connect>`, plays the tones, then reconnects a fresh
Media Stream to the same call/session. The adapter must therefore be **reconnect-safe**:
streams are attached/detached; `start` may happen more than once per call; `stop` is not an
end. Consequences to document: a short audio gap, one extra stream SID, ~0.5–1 s latency.

Validation: digits must match `^[0-9A-D*#wW]{1,32}$` (Twilio `<Play digits>` alphabet;
`w`/`W` are 0.5 s/1 s pauses), and the call must be answered (`ErrSessionNotReady`
otherwise). The TwiML is built with `encoding/xml` (escaping is mandatory).

**Decision gate (live test):** if a real call shows that the call update ends the call or the
stream cannot be re-established, do **not** fake success. Return an exported
`ErrDTMFUnsupported`, keep inbound DTMF working, record the finding in PROGRESS, and move
outbound DTMF to C4 (where barge-in/IVR handling lives). The port method still exists; it
just refuses cleanly.

### 3.6 Webhook authenticity

Twilio signs form-encoded webhooks with `X-Twilio-Signature`:

```
signed = URL + concat(sorted(param names+values))
sig    = base64(HMAC-SHA1(authToken, signed))
```

Implementation rules:

- `URL` is **`PublicBaseURL + r.URL.EscapedPath()`** plus `?rawQuery` if non-empty (our URLs
  have no query). It must be the exact URL configured in Twilio — not `r.Host`, which is the
  internal/tunnel host. This is the #1 tunnel bug; the startup log prints the configured
  public URL so mismatches are visible.
- Params are **all** `r.PostForm` values (body only; the URL already contains the query).
  Use `r.ParseForm()` after `MaxBytesReader`; do not pick a fixed allowlist — Twilio adds
  parameters without notice.
- Compare with `hmac.Equal`/`subtle.ConstantTimeCompare`; malformed/empty signature → 403.
- Accept the signature computed with and without an explicit default port (`:443` for
  https), mirroring Twilio's own validator (`addPort`/`removePort`), because Twilio's
  backend is inconsistent about it.
- No Twilio SDK: Twilio recommends their validator library, but adding another dependency is
  not justified. Correctness is pinned by Twilio's published test vector (see §7).
- The **WebSocket handshake is not signed**. Protect it with: unguessable 32-hex call ID in
  the path, `start.accountSid == configured AccountSID`, `start.customParameters["call_id"]
  == path ID`, and the expected `mediaFormat` (`audio/x-mulaw`, 8000, 1). Any mismatch →
  close `1008` and log. Unknown/ended call → HTTP `404` before upgrade.
- Newer "shared key" webhooks may advertise HMAC-SHA256 via
  `X-Twilio-Webhooks-Signature-Settings`; C1 uses the account auth token (HMAC-SHA1).
  If that header is present, log a warning and reject (fail closed).

### 3.7 Session lifecycle and concurrency

- `Adapter` holds `mu sync.Mutex` + `sessions map[domain.CallID]*session`. Register **before**
  the REST create so a fast callback cannot race; remove on terminal end.
- `Dial`: validate phone (`^\+[1-9][0-9]{6,14}$`), create ID, register, POST, emit
  `EventDialing`, return the session. On POST failure: unregister, close channels, return a
  wrapped error (no partial session).
- One session = one call, but **possibly several streams** (DTMF reconnect, Twilio reconnect).
  The current stream connection is stored under a mutex; a new `start` replaces it and closes
  the old one.
- Goroutines per session: status/event dispatcher, outbound audio writer, one reader per
  WebSocket. All exit on `done`. `Events`/`Inbound` are closed by the dispatcher only.
- All progress emission goes through a `post` that is a no-op once `done` is closed, so a
  terminal callback that races the REST create response cannot emit `dialing` after `ended`.
- `Hangup`: idempotent; end locally first (`EndAborted`), then `POST Calls/<sid>.json
  Status=completed` best-effort (short timeout; log failures). Safe after natural end.
- Safety net: `time.AfterFunc(30*time.Minute)` per live session → log, `end(EndError)`
  locally, then best-effort REST end. Prevents registry/channel leaks if webhooks never
  arrive. (C5 may add a proper per-call time limit.)
- Delivery guard: the terminal `EventEnded` send waits at most **5 s** for a consumer;
  then the channels are closed with a warning. A conforming consumer drains, so this only
  fires on a bug — it must never deadlock a webhook or `Hangup`.
- HTTP handlers never block on the far end: the REST client has a 20 s timeout; webhook
  processing is local and returns 200/403 quickly. Event volume is tiny; the dispatcher
  delivers in order and can block only on a non-draining consumer (which the port contract
  forbids).

### 3.8 Errors and logging

Adapter-local sentinels (exported for tests and the future runner):

```go
var (
    ErrSessionEnded    = errors.New("call session ended")
    ErrSessionNotReady = errors.New("call session not ready") // DTMF before answer / no SID
    ErrDTMFUnsupported = errors.New("outbound dtmf is not supported") // only if the gate fails
)

type APIError struct {
    StatusCode int    // HTTP status (400, 401, 404, 429, 500…)
    Code       int    // Twilio error code, e.g. 21215
    MoreInfo   string // Twilio docs URL
    Message    string // provider text; log-only, never returned to clients
}
```

- `Error()` on `APIError` contains status + code only; `Message`/`MoreInfo` are logged at
  debug. Never log `AuthToken`, `AccountSID`, or full phone numbers. Log keys: `call_id`,
  `provider_call_sid`, `stream_sid`, `event`, `status`, `reason`, `digits`, `error`.
- `Dial` does **not** retry. Retry/backoff is C5's scheduler policy.
- Context: use `ctx` for the REST create only; after `Dial` returns, cancellation of that
  context must not end the call (the caller owns the session and calls `Hangup`). Document
  this in the adapter code.

---

## 4. Layout and dependency law

New package (all files new):

```
internal/adapters/telephony/twilio/
├── adapter.go       Config, New, Adapter, Dial, registry, Handler()
├── client.go        REST create/update, APIError, basic auth
├── session.go       CallSession: progress ordering, event dispatch, audio queues, Hangup
├── webhook.go       mux: voice / status / stream-status, signature gate, body limits
├── stream.go        WebSocket accept + read loop + Twilio JSON envelopes
├── twiml.go         XML builders (Connect/Stream/Parameter, Play digits, Hangup)
├── signature.go     HMAC-SHA1 validator (+ port variants)
├── audio.go         µ-law encode/decode, 8↔16 kHz, 20 ms framing
├── audio_test.go
├── signature_test.go
├── twiml_test.go
├── client_test.go
├── session_test.go
├── stream_test.go
├── webhook_test.go
└── (integration_test.go for the full fake-Twilio flow)
```

Modified files:

```
internal/core/config/config.go        + PublicURL, RingTimeoutSeconds, validation, Summary
internal/core/config/env.go          + 2 registry entries
internal/core/config/config_test.go  + cases; update Summary key list
internal/app/app.go                  + Webhooks http.Handler field
internal/app/providers.go            + real twilio case, handler return
internal/app/app_test.go             update "twilio not implemented" expectation
internal/app/live_test.go            new, //go:build live
internal/httpapi/server.go           mount a.Webhooks
internal/httpapi/server_test.go      webhook route visible only for twilio; 403 unsigned
go.mod / go.sum                       + github.com/coder/websocket v1.8.15
.github/workflows/{ci,release}.yml    cache-dependency-path -> go.sum
Makefile                              + live-test target
README.md, docs/architecture.md, docs/api.md, docs/twilio.md, PROGRESS.md
```

**Untouched:** `internal/core/domain`, `internal/core/ports`. No port change is needed; if
you think one is, stop — that is a design change.

Import law remains:

```
internal/adapters/* → internal/core/{ports,domain} (+ vendored/vetted third-party libs)
internal/httpapi     → core/{ports,domain,config} + app; never adapters
internal/app         → core/{config,ports,domain} + adapters   (composition root only)
```

The adapter gets its settings as a local `twilio.Config` mapped by `internal/app`; it must
**not** import `internal/core/config`.

---

## 5. Config surface changes

### 5.1 New fields

```go
type HTTP struct {
    Addr      string
    PublicURL string // public https origin of this server; required for voice-api webhooks
}

type Twilio struct {
    AccountSID         string // secret
    AuthToken          string // secret
    PhoneNumber        string // E.164 caller id
    Mode               string // "voice-api" | "sip-trunk"
    RingTimeoutSeconds int    // 5..600, default 30
}
```

### 5.2 New environment variables

| Variable | Default | Notes |
|---|---|---|
| `INSTEADLY_HTTP_PUBLIC_URL` | — | Public **https** origin Twilio reaches (tunnel URL), e.g. `https://abc.trycloudflare.com`; no path, query, or fragment; a trailing `/` is tolerated and trimmed. Required when `telephony.provider=twilio` **and** `mode=voice-api`. |
| `INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS` | `30` | Twilio `Timeout` parameter; validate 5–600. |

Validation (first error wins, `config: ` prefix, no secret values):

- `PublicURL`, when non-empty: absolute `https`, host non-empty, path empty or `/`, no
  query/fragment. (`http` is rejected; tests construct `twilio.Config` directly.)
- `telephony.provider=twilio`:
  - `account_sid` matches `^AC[0-9a-fA-F]{32}$`;
  - `auth_token` non-empty;
  - `phone_number` matches `^\+[1-9][0-9]{6,14}$`;
  - `mode=voice-api` requires `PublicURL`; `sip-trunk` does not (no webhooks) but the
    **builder** returns `ErrNotImplemented` (C7).
- `ring_timeout` 5–600.

### 5.3 No legacy-env fallback (explicit decision)

The existing `.env` uses the standard unprefixed Twilio names. C1 **keeps** the C0 rule that
only `INSTEADLY_*` variables are read: no special cases, no surprise overrides from other
tools' `TWILIO_*` variables, and `TestNonInsteadlyEnvIgnored` stays valid. To run the
adapter, either add the prefixed lines to `.env` (user action — the agent never edits `.env`)
or export them:

```bash
export INSTEADLY_TWILIO_ACCOUNT_SID="$TWILIO_ACCOUNT_SID"
export INSTEADLY_TWILIO_AUTH_TOKEN="$TWILIO_AUTH_TOKEN"
export INSTEADLY_TWILIO_PHONE_NUMBER="$TWILIO_PHONE_NUMBER"
export INSTEADLY_HTTP_PUBLIC_URL="https://<tunnel>.trycloudflare.com"
export INSTEADLY_TELEPHONY_PROVIDER=twilio
```

If the user prefers zero-touch `.env`, a future amendment can add an explicit allowlisted
fallback; it is not in C1.

### 5.4 Summary

Add non-secret keys (never AccountSID/AuthToken/PhoneNumber):

```go
"http.public_url":                       c.HTTP.PublicURL,
"telephony.twilio.mode":                 c.Telephony.Twilio.Mode,
"telephony.twilio.ring_timeout_seconds": c.Telephony.Twilio.RingTimeoutSeconds,
```

`TestSummaryContainsNoSecrets` must be updated for the new key count.

---

## 6. Implementation steps (do them in this order)

### Step 0 — Dependency + CI plumbing

1. ```bash
   go get github.com/coder/websocket@v1.8.15
   go mod tidy
   ```
   `go.mod` gains exactly one `require`; `go.sum` appears. Pin v1.8.15 (latest observed
   2026-06-15).
2. Update `.github/workflows/ci.yml` (three jobs) and `release.yml`: replace
   `cache-dependency-path: go.mod` with `cache-dependency-path: go.sum` (setup-go caches by
   module sums). `GOFLAGS: -mod=readonly` stays.
3. Verify: `go build ./...`, `go test -count=1 ./...`, `git status` shows `go.mod`/`go.sum`.
   Optionally `actionlint` on the workflows (installed 1.7.12 previously).

### Step 1 — Config

1. Add `HTTP.PublicURL`, `Twilio.RingTimeoutSeconds`; defaults (`30`). In `load`, trim a
   trailing `/` from `PublicURL` before validation (and keep the adapter's defensive trim).
2. Registry entries + `intSetter` for the timeout (parse errors name the variable, never a
   secret).
3. Validation as in §5.2; `Summary` keys as in §5.4. Keep "first error wins".
4. Tests: defaults; valid/invalid public URLs (http, query, path, trailing slash); twilio
   required fields (SID pattern, token, E.164); ring timeout bounds; summary keys and no
   secret values; `TestNonInsteadlyEnvIgnored` unchanged.
5. Verify: `go test ./internal/core/config/`; `go build ./...`.

### Step 2 — Audio codec (`audio.go`)

1. `decodeMuLaw(byte) int16` / `encodeMuLaw(int16) byte` (G.711, standard tables/algorithm).
2. `upsample8to16([]int16) []int16` (linear interpolation).
3. `downsample16to8([]int16) []int16` (9-tap low-pass + decimate; coefficients in code).
4. `frameEncoder`: accumulate canonical PCM bytes, emit whole 20 ms (160-byte µ-law)
   frames; `frameDecoder`: accumulate µ-law samples, emit whole 640-byte canonical chunks.
   Enforce little-endian via `encoding/binary`.
5. Tests per §7; no I/O. Verify `go test ./internal/adapters/telephony/twilio/`.

### Step 3 — Signature (`signature.go`)

1. `validSignature(authToken, requestURL, signature string, params url.Values) bool`
   implementing §3.6, including the default-port variants and constant-time compare.
2. `signedURL(r *http.Request, publicBaseURL string) string`:
   `publicBaseURL + r.URL.EscapedPath()` (+ raw query).
3. Tests: Twilio's published vector; tampered param/URL/signature; empty signature; wrong
   token; port variant; percent-encoded path.
4. Verify: package tests.

### Step 4 — TwiML (`twiml.go`)

1. XML structs for:
   - `<Response><Connect><Stream url="wss://…" statusCallback="https://…"><Parameter name="call_id" value="…"/></Stream></Connect></Response>`
   - DTMF variant with `<Play digits="…"/>` before `<Connect>`.
   - `<Response><Hangup/></Response>` for ended/unknown sessions.
2. Helpers: `wsURL(publicBaseURL, callID)` (https→wss, http→ws for tests only),
   `voiceURL`, `statusURL`, `streamStatusURL`.
3. `text/xml` + `Cache-Control: no-store`, `xml.Header`.
4. Tests: parse result, assert exact URL/path, no query string, escaping of IDs, status
   callback present.

### Step 5 — REST client + `Dial` (`client.go`, `adapter.go`)

1. `Config`/`New` with defaults; `strings.TrimRight(PublicBaseURL, "/")` so a tolerated
   trailing slash cannot produce `//webhooks` URLs (which would break signatures); validate
   public URL scheme (http only for tests, warn), credentials, ring timeout, API base.
2. `createCall(ctx, params)` → `POST {apiBase}/2010-04-01/Accounts/{sid}/Calls.json`, basic
   auth, `url.Values` with:
   `To`, `From`, `Url`, `Method=POST`, `StatusCallback`,
   `StatusCallbackMethod=POST`,
   `StatusCallbackEvent=initiated|ringing|answered|completed` (repeated keys),
   `Timeout=<seconds>`, `Record=false`.
3. Parse `{sid,status}`; non-2xx → `*APIError` (parse `{code,message,more_info}` when JSON).
4. `updateCall(ctx, sid, twiml)` → `POST …/Calls/{sid}.json` with `Twiml=…` (DTMF).
5. `Dial`: validate phone, create/register session, call `createCall`, require a non-empty
   `sid` from the response, set it, emit `Dialing`, return. On any error: unregister + close
   + wrapped error.
6. Tests with `httptest`: form values, basic auth, repeated events, timeout, error mapping,
   no session leak, context cancellation.

### Step 6 — Session (`session.go`)

1. State: ID, SID, current stream, progress rank, answered/ended flags, `endOnce`, reason,
   event queue/dispatcher, inbound (cap 100, drop-oldest), outbound (cap 100), done, reaper.
2. Event dispatcher: ordered delivery; on end, emit `Ended` last, close `Events` and
   `Inbound`; implement the "synthesize missing progress" rules from §3.3.
3. `SendAudio`: queue + writer goroutine; wait for stream; 20 ms framing via `audio.go`;
   error after end; ctx-aware.
4. `SendDTMF`: validation + call update (Step 9); error before answer.
5. `Hangup`: local end first, REST best-effort, idempotent.
6. Reaper: 30 min.
7. Tests: ordering table, end-once, channel closure, SendAudio after end, concurrent
   SendAudio/Hangup under `-race`, drop-oldest.

### Step 7 — Webhooks (`webhook.go`)

1. `Handler()` returns a `http.Handler` with its own `http.ServeMux` and the full-path
   patterns from §3.2.
2. Middleware inside the adapter: body limit, signature check for the three HTTP routes,
   and request correlation. The adapter must not import `httpapi`; `httpapi` already sets
   the `X-Request-Id` **response** header before calling the handler, so read
   `w.Header().Get("X-Request-Id")` and log it when present.
3. Voice: lookup session; build TwiML; ended → Hangup TwiML; unknown → 404.
4. Status: parse form; map per §3.3; always 200 after signature.
5. Stream status: log `stream-started`/`stream-stopped`/`stream-error`; 200.
6. Tests: valid/invalid signatures, unknown IDs, ended call, oversized body, no-op duplicate
   callbacks, out-of-order callbacks.

### Step 8 — WebSocket (`stream.go`)

1. `GET` route: lookup session (404 if unknown/ended), `websocket.Accept` (default origin
   policy is fine: Twilio sends no `Origin`), `defer CloseNow`.
2. Read loop with `wsjson.Read`/`json.Unmarshal` of the envelope:
   `connected`, `start`, `media`, `dtmf`, `mark`, `stop`; ignore unknown events. Set a
   read limit (64 KiB) and give each write a short deadline (5 s).
3. Validate `start` per §3.6 **before** attaching; on failure close 1008 and log.
4. Attach stream (replacing/old closing any previous); event `Answered`; media → inbound
   pipeline; dtmf → event; stop → detach only.
5. Writer integration: current stream under mutex; `streamSid` from `start`; write `media`
   text frames; on write error detach, don't end.
6. Tests: fake Twilio client using `websocket.Dial` against `httptest`; full flow; reconnect;
   wrong AccountSID/call_id/format closes; malformed JSON; audio byte checks; send-audio
   frames.

### Step 9 — Outbound DTMF

1. Build the Play+Connect TwiML; `updateCall`.
2. Validate digits and answered state; return `ErrSessionNotReady` otherwise.
3. Test: fake REST asserts the TwiML; old stream closes, new stream attaches, no `Ended`
   event; concurrent `SendDTMF` + `Hangup` is safe.
4. Live gate per §3.5.

### Step 10 — Wiring (`app`, `httpapi`)

1. `App` gains `Webhooks http.Handler` (nil for stub).
2. `buildTelephony(c config.Telephony, publicURL string, log *slog.Logger) (ports.Telephony, http.Handler, error)`:
   - `stub` → `stub.NewTelephony(log)`, nil handler;
   - `twilio` + `mode=voice-api` → `twilio.New(...)`, `a.Handler()`;
   - `twilio` + `sip-trunk` → `ErrNotImplemented` (C7);
   - unknown → error.
3. `httpapi.New`: `if a.Webhooks != nil { mux.Handle("/webhooks/telephony/twilio/", a.Webhooks) }`.
4. Update `app_test.go`: twilio now builds (with `PublicURL`, valid SID/token/E.164);
   sip-trunk still `ErrNotImplemented`; mistral cases unchanged; `testConfig` gains
   `PublicURL: ""`.
5. `httpapi` tests: stub → webhook path 404; twilio config → unsigned voice webhook 403
   (proves mounting); existing API tests unchanged.
6. Verify: `go build ./...`, `go test -race ./...`, `make check-deps` (must still print
   `OK: core imports no adapters`).

### Step 11 — Docs

1. `docs/twilio.md`: console checklist (number, verified caller, trial geo), env vars,
   tunnel, route table, signature troubleshooting (403 = URL mismatch), error codes
   (21215 geo permissions, 21210 caller ID not verified, 11200 HTTP failure), known DTMF
   gap, live-test recipe.
2. `docs/architecture.md`: Twilio adapter section (sequence, routes, audio pipeline,
   reconnect/DTMF, session lifecycle); update composition root bullet; note the one
   dependency.
3. `docs/api.md`: "Telephony webhooks" section (not part of the JSON API; only mounted for
   twilio; signature required).
4. `README.md`: C1 status, new env vars, dependency note, `docs/twilio.md` link,
   `make live-test` entry.
5. `Makefile`: `live-test` target (build tag `live`, not part of `verify`).

### Step 12 — Live verification + PROGRESS

Follow §8. Record exact commands/results in PROGRESS, update the chunk table and "Status at
a glance".

---

## 7. Test plan (file → cases)

| Test file | Cases |
|---|---|
| `config/config_test.go` | defaults (public URL empty, ring 30); public URL must be https/host/no path/query; trailing slash tolerated; twilio requires SID/token/E.164/public URL (voice-api); sip-trunk does not require public URL; ring timeout bounds; summary key list + no secret values; existing tests unchanged |
| `twilio/audio_test.go` | µ-law decode known values (`0x00→-32124`, `0x7F→0`, `0x80→32124`, `0xFF→0`); encode∘decode == identity for all 256; 8→16 k interleaving/length; 16→8 k length + 6 kHz attenuation; frame decoder/encoder with arbitrary chunk sizes and residual |
| `twilio/signature_test.go` | Twilio vector (token `12345`, URL `https://mycompany.com/myapp.php?foo=1&bar=2`, params, sig `vOEb5UThFn24KEfnOFLQY2AE5FY=`); tampered param/URL/sig; empty; wrong token; port-with/without variants; percent-encoded path; form values with multiple keys |
| `twilio/twiml_test.go` | voice TwiML parses and contains `wss://…/stream/<id>`, `<Parameter name="call_id">`, status callback; no query string; https→wss and http→ws; DTMF TwiML has `<Play digits>` before `<Connect>`; ended TwiML is `<Hangup>`; XML escaping |
| `twilio/client_test.go` | create-call form: To/From/Url/Method/StatusCallback/repeated StatusCallbackEvent/Timeout/Record; basic auth; 201 parse; 400/401/429 → `*APIError` with code; network error wrapped; no session left registered; Dial invalid phone; context cancellation |
| `twilio/session_test.go` | progress table (§3.3) incl. out-of-order + duplicates; Answered/Ended once; Ended last + channels closed; Hangup idempotent + REST called once; SendAudio queues/streams; SendAudio after end → `ErrSessionEnded`; drop-oldest cap 100; concurrent SendAudio/Hangup under `-race`; 30 min reaper (inject clock/timer or short test override) |
| `twilio/stream_test.go` | fake Twilio WS client (`websocket.Dial` + `wsjson`): connected/start → answered; inbound media (160 B µ-law) → 640 B PCM16; dtmf → EventDTMF; stop does not end; malformed payload/JSON; wrong AccountSID/call_id/format close 1008; reconnect replaces stream and audio follows the new one; outbound media frames decode to the expected samples |
| `twilio/webhook_test.go` | signed voice → 200 XML; unsigned/tampered → 403; unknown/ended call IDs; signed status → events and 200; unknown status → 200; duplicate/out-of-order; body > 64 KiB → 413; stream-status logs; **WS upgrade through `httpapi` middleware** |
| `twilio/integration_test.go` | one fake end-to-end: fake REST + `httptest` server + fake WS → `Dial` → callbacks → audio in/out → DTMF update → completed; assert no provider concepts leak, `go test -race` clean |
| `app/app_test.go` | twilio builds with valid config (Webhooks non-nil); stub has nil webhooks; sip-trunk → `ErrNotImplemented`; mistral unchanged |
| `httpapi/server_test.go` | stub → `/webhooks/telephony/twilio/...` 404; twilio → unsigned voice 403; existing API behavior unchanged |
| `app/live_test.go` (`//go:build live`) | real call per §8.6; skipped unless `INSTEADLY_LIVE_TWILIO=1`; requires `TEST_PHONE`, tunnel URL, creds; asserts dialing/ringing/answered/ended and sends a tone (+ DTMF when `INSTEADLY_LIVE_DTMF=1`) |

Run `make test` (`-race`) plus `make lint`, `make check-deps`, `make build`; all green.
Aim for ≥ 80 % statements in the twilio package (audio, signature, ordering are the
important parts); overall coverage must not regress materially from 83.8 %.

---

## 8. Definition of done (exit criteria — record evidence in PROGRESS)

1. **Build/test:** `make verify` green (gofmt, `go vet`, `go test -race -count=1 ./...`,
   `make check-deps` → `OK: core imports no adapters`, `make build VERSION=c1`).
2. **No domain/port changes:** `internal/core/{domain,ports}` unchanged; the only core
   changes are
   §5. `go.mod` has exactly one new `require`; `go.sum` committed.
3. **Fake-Twilio proof (CI-runnable, no credentials):** the integration test drives
   Dial → webhooks → WS → audio in/out → DTMF → ended, and the unit tests cover the tables
   in §7.
4. **Security:** unsigned and tampered webhooks → 403; WS with wrong AccountSID/call_id/
   format closed; 64 KiB body cap; no secret in any log line (grep the test output for the
   fake token).
5. **Boot matrix:**
   - stub provider (no env) → boots, healthz providers still stub (regression);
   - twilio + full valid config + a dummy `https://example.test` public URL → boots,
     healthz `telephony: twilio`, webhook routes mounted (no call made);
   - twilio missing public URL / token / valid E.164 → exit 1 with a clear `config:` error;
   - twilio `mode=sip-trunk` → exit 1 `not implemented (lands in chunk C7)`;
   - `INSTEADLY_HTTP_PUBLIC_URL=http://…` → exit 1.
6. **Real call (user-approved, costs credit):**
   ```bash
   # terminal 1
   cloudflared tunnel --url http://127.0.0.1:8080     # note https URL

   # terminal 2
   set -a; . ./.env; set +a
   export INSTEADLY_TWILIO_ACCOUNT_SID="$TWILIO_ACCOUNT_SID"
   export INSTEADLY_TWILIO_AUTH_TOKEN="$TWILIO_AUTH_TOKEN"
   export INSTEADLY_TWILIO_PHONE_NUMBER="$TWILIO_PHONE_NUMBER"
   export INSTEADLY_TELEPHONY_PROVIDER=twilio
   export INSTEADLY_HTTP_PUBLIC_URL="https://<tunnel>.trycloudflare.com"
   export INSTEADLY_LIVE_TWILIO=1
   export INSTEADLY_LIVE_DTMF=1   # optional: exercises the redirect-based SendDTMF
   make live-test
   ```
   The live test runs from the `internal/app` package directory, so it must **not** rely on
   `./.env` being found there; the recipe above exports every value it needs. Approve the
   `cloudflared` install (if not already present) and the trial call before running.

   Evidence: the owner's phone rings, hears a ~1 s 440 Hz tone (and a DTMF tone if
   enabled), then the call ends; the test logs `dialing → ringing → answered → ended`
   exactly once; no 403/11200 in the adapter logs. If this is blocked by a missing tunnel
   or console restriction, **do not mark C1 ✅** — record the exact blocker and keep it 🔵/⛔.
7. **Docs:** README env table + dependency + live recipe; architecture; API webhooks;
   `docs/twilio.md`; PROGRESS updated (status, chunk row, session log, env facts).
8. **Hygiene:** no `.env` edits, no secrets/phone numbers printed, `git status` shows only
   expected files; no commit unless asked.

---

## 9. Risks and decision gates

| Risk | Mitigation / gate |
|---|---|
| Tunnel URL changes or mismatches → all webhooks 403 and calls fail with 11200 | Config validation + startup log of `http.public_url`; docs; rebuild/restart on tunnel change |
| Trial account restrictions (verified numbers, geo permissions) | Precondition check; surface Twilio `APIError` code (21215/21210) in logs; user fixes console |
| `cloudflared` not installed; no public endpoint | User approval to install, or alternative tunnel; live exit criterion stays blocked until available |
| Outbound DTMF redirect ends the call / cannot reconnect | **Decision gate:** live test; fallback to `ErrDTMFUnsupported` + inbound DTMF only, C4 follow-up; never claim success |
| Status callbacks out of order/duplicated (documented by Twilio) | Monotonic rank + idempotent end-once + tests |
| WebSocket upgrade through `statusRecorder` fails | `Unwrap()` is already implemented; explicit integration test for 101 |
| Audio aliasing / silence / wrong rate | Known µ-law vectors, sine tests, byte-length assertions; speech quality validated in C2/C3 |
| New third-party dependency | Pinned v1.8.15, zero transitive deps, MIT, CI go.sum, `go mod tidy` check |
| Session/channel leak if webhooks never arrive | 30 min reaper, terminal removal, `-race` tests |
| Slow consumer blocks webhooks | Event volume is tiny; dispatcher; status handler returns after local work; audio uses bounded queues/drop-oldest |
| Recording/PII creep | C1 logs no phone numbers and no secrets; `Record=false` until C7 |

---

## 10. Expected size & effort

Roughly **1,000–1,400 lines** of adapter code + tests, plus ~150 lines of docs/config/CI
changes. Two focused sessions: (1) codec, signature, TwiML, REST, session + fake tests;
(2) WebSocket, webhooks, wiring, live call, docs. The protocol code is the slow part; copy
the message shapes from §3 and the Twilio docs, not from memory.

---

## 11. What C2/C3 plug into here

- **C2 (Mistral TTS):** emits canonical 640-byte chunks → `CallSession.SendAudio`; the
  adapter handles 16→8 kHz + µ-law and stream reconnects. No Twilio knowledge in C2.
- **C2 (Mistral STT):** consumes `Inbound()` after the core's VAD; the adapter already
  delivers canonical 20 ms frames at 16 kHz.
- **C3 (call runner/agent):** calls `Telephony.Dial`, consumes `Events()`/`Inbound()`,
  drives the state machine (`dialing → ringing → in_call → finished`), and calls `Hangup`.
  The webhook routes are already mounted in the same binary.
- **C4 (conversation):** barge-in needs `clear`/`mark` support (add to the adapter then);
  outbound DTMF may be revisited if the redirect gate failed; VAD stays in the core.
- **C7 (swapability):** `telephony.twilio.mode=sip-trunk` and a second provider are new
  adapter packages/cases behind the same port; nothing in the core changes.

---

## Sources consulted (2026-11-17)

- Twilio Media Streams — WebSocket messages (connected/start/media/dtmf/stop/mark; server
  may send only media/mark/clear): <https://www.twilio.com/docs/voice/media-streams/websocket-messages>
- Twilio Media Streams overview: <https://www.twilio.com/docs/voice/media-streams>
- TwiML `<Connect>`: <https://www.twilio.com/docs/voice/twiml/connect>
- TwiML `<Stream>` (url has no query string; `<Parameter>`; statusCallback; bidirectional
  only inbound track): <https://www.twilio.com/docs/voice/twiml/stream>
- TwiML `<Play digits>` (DTMF alphabet and pauses): <https://www.twilio.com/docs/voice/twiml/play>
- Twilio Call resource (create/update, `StatusCallbackEvent`, status values, `SendDigits`,
  `Timeout`): <https://www.twilio.com/docs/voice/api/call-resource>
- Twilio webhook security / signature algorithm: <https://www.twilio.com/docs/usage/webhooks/webhooks-security>
- Twilio Go SDK validator test vectors (HMAC-SHA1, token `12345`):
  <https://github.com/twilio/twilio-go/blob/main/client/request_validator_test.go>
- `coder/websocket` v1.8.15 (zero-dependency, `Unwrap`-aware hijacking):
  <https://github.com/coder/websocket>

---

## Amendments

### 2026-11-17 — implementation findings

1. **Live audio blocked by the Twilio trial.** Twilio strips the `<Stream>` verb from
   custom TwiML on trial accounts and substitutes a "not available on trial accounts"
   message. A real call was made end to end (dialing → ringing → answered → TwiML fetch →
   completed) and status callbacks/TwiML fetching work, but no Media Stream attached.
   Media Streams requires an upgraded account. The live test now fails explicitly if
   `StreamReady()` never fires, instead of passing on silence.
2. **Trial Calls API rejects optional parameters** ("trial accounts have limited parameter
   access"). The adapter now sends only `To`, `From`, `Url`, `StatusCallback`, and
   `StatusCallbackEvent`; `Method`, `StatusCallbackMethod`, and `Record` are omitted (their
   defaults are what we want) and `Timeout` is opt-in. `RingTimeoutSeconds` therefore
   defaults to `0` (Twilio's ~60 s) instead of 30 s.
3. **The TwiML `Url` fetch is not signed.** Live requests carried no
   `X-Twilio-Signature` (status callbacks did). The voice route now accepts an unsigned
   fetch only for a known call ID with matching `AccountSid`/`CallSid`; if a signature is
   present it is still validated. The `<Stream statusCallback>` route is diagnostic and
   accepts unsigned requests as well (it only logs).
4. **µ-law zero canonicalization:** `encode(decode(0x7f)) == 0xff`; the test asserts
   sample-level round-trips plus this one canonicalization instead of byte identity for all
   256 codes.
5. **Live-test flags are unprefixed:** `LIVE_TWILIO=1` / `LIVE_DTMF=1`, because strict
   config loading rejects unknown `INSTEADLY_*` names.
6. **`StreamReady()`** is an adapter-specific diagnostic hook (not a port method) used by
   the live test to distinguish "trial stripped `<Stream>`" from "audio is silent".
7. **CI cache path:** `cache-dependency-path` now points at `go.sum`; the one new module is
   `github.com/coder/websocket v1.8.15`.
