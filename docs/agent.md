# Call runner + agent engine (C3)

C3 is the first provider-neutral **execution** path. Two core packages own it:

```
internal/core/runner   one order attempt: claim, dial, state machine, hangup, final outcome
internal/core/agent    answered-call loop: greeting, listen, STT, dialogue/tools, TTS
```

Both import only `internal/core/{ports,domain}` (plus stdlib/`log/slog`; the runner also
imports the agent). `internal/app` is the composition root and the only package that wires
them to concrete adapters. No adapter imports the runner/agent, and no public HTTP call
endpoint exists in C3 (see [Invocation](#invocation-no-public-call-endpoint)).

## Ownership

| Concern | Owner | Does not own |
|---|---|---|
| `Orders.Get`, `scheduled → dialing`, `Attempts++`, one `Dial` | runner | scheduling, retries, payment |
| Consuming `CallEvent`, order transitions, answered/ended signaling | runner | STT/TTS/dialogue policy |
| Final outcome selection, exactly one `Finish`, best-effort `Hangup` | runner | model parsing |
| Greeting/audio/dialogue/tool loop, M0 utterance boundaries | agent | dialing, order transitions, Hangup, storage finalization |
| Tool schemas/validation/execution, per-call booking state | agent | `finish_call` (adapter-intercepted), human transfer |
| Prompt/outcome schema | agent | provider model names/voice selection |
| Construction/DI | `internal/app` | conversation/business rules |
| Provider protocols, audio conversion | existing C1/C2 adapters | core state/tools/policy |

The agent receives a narrow `AudioSession` (`Inbound`, `SendAudio` only), so it cannot
consume the runner's event channel, send DTMF, or hang up.

## Runner sequence

```
caller (test/future scheduler)   runner                        telephony / agent
        │ Run(order_id)           │                              │
        │────────────────────────►│ Orders.Get                   │
        │                         │ Update: scheduled→dialing,   │
        │                         │         Attempts++ (once)    │
        │                         │ Dial(callCtx, order) ───────►│
        │                         │◄────────────── CallSession    │
        │                         │ drain events, drive statuses  │
        │                         │ wait optional StreamReady     │
        │                         │ Agent.Run(callCtx, ...,ended)►│
        │                         │◄──────── outcome or error ────│
        │                         │ Hangup(cleanupCtx), drain end │
        │                         │ Update: Finish(outcome)       │
        │◄──────── final order ───│                              │
```

Rules implemented:

1. **Claim before dial.** One atomic `Orders.Update` requires `scheduled`, transitions to
   `dialing`, and increments `Attempts`. A second `Run` sees a non-scheduled status and
   fails with `domain.ErrInvalidTransition` before dialing (prototype-local duplicate
   protection; C5 owns distributed leases).
2. **One call-lifetime context.** `Run` creates a 6-minute `callCtx` (configurable test
   knob) and passes it to both `Dial` and `Agent.Run`; it is not canceled after setup, so a
   stub-style session that selects on its context survives. An internally expired call
   deadline maps to `failed/other`; caller cancellation and `Runner.Close` map to `aborted`.
3. **Event-driven statuses.** Before answer only the runner consumes events. Duplicate
   `dialing` is a no-op; duplicate `ringing`/`answered` are no-ops; skipped/regressed
   events fail the attempt. `EventDTMF` is debug-logged only (no IVR policy in C3).
4. **Optional readiness.** Before starting the agent, the runner type-asserts
   `interface{ StreamReady() <-chan struct{} }` (C1 exposes it on the Twilio session) and
   waits up to 20 s while still draining events. A timeout is `failed/error`; absence of
   the hook starts immediately. This prevents greeting audio from racing C1's
   `ErrSessionNotReady`.
5. **Post-answer relay.** One goroutine drains events and forwards the terminal
   `EventEnded` on a buffered channel to the agent; it does not transition orders. It is
   the only post-answer consumer.
6. **Cleanup.** The runner calls `Hangup` exactly once after a successful `Dial`, with a
   fresh bounded context (`context.WithoutCancel`), then drains the terminal event for at
   most the cleanup timeout. Hangup/drain failures are logged and never overwrite a valid
   dialogue outcome.
7. **Exactly one persistence.** Finalization is one `Orders.Update` with a fresh bounded
   context that applies `Order.Finish(outcome)` to the latest stored order (preserving
   captured patient fields). There is no retry loop; a storage failure returns the last
   known order plus the error and never claims the order finished.
8. **Shutdown.** `Runner.Close` atomically rejects new runs, cancels every registered
   active call, and waits until cleanup or the caller's context expires. `App.Close`
   delegates to it; `cmd/insteadly` closes the runner **before** `http.Server.Shutdown`
   so active calls still have the telephony HTTP/WebSocket surface while they hang up.

`Run` is synchronous. C5's scheduler can call it directly (one goroutine per call).

## Events → statuses → outcomes

| Input | Runner behavior |
|---|---|
| claim | require `scheduled`; `scheduled → dialing`; `Attempts++` once |
| `EventDialing` in `dialing` | no-op |
| `EventDialing` elsewhere | invalid-transition error; attempt fails |
| `EventRinging` in `dialing` | `dialing → ringing` |
| `EventRinging` in `ringing` | duplicate no-op |
| `EventAnswered` in `ringing` | `ringing → in_call`; start the agent once |
| duplicate `EventAnswered` in `in_call` | no-op; never a second agent |
| `EventDTMF` | debug log only |
| `EventError` | mark provider failure; keep draining to `EventEnded` |
| `EventEnded` | record reason; no `Transition(finished)` |
| channel closes without `EventEnded` | provider-contract failure → `failed/error` |
| skipped/regressed progress | invalid-transition error; one attempt fails closed |

Outcome precedence: a **valid dialogue outcome always wins** over the local Hangup reason
(usually `EndAborted`). Otherwise:

| Situation without a dialogue outcome | Final outcome |
|---|---|
| caller cancellation or `Runner.Close` | `aborted`, generic detail |
| `EndAborted` | `aborted` |
| `EndBusy` / `EndNoAnswer` / `EndRejected` | `failed` with `busy` / `no_answer` / `rejected` |
| `EndIVRDeadEnd` | `failed` with `ivr_dead_end` (mapping only) |
| `EndError`, `EventError`, dial/agent failure | `failed` with `error` |
| `EndCompleted` before an outcome | `failed` with `other` |
| listen/silence/tool-round/user-turn/call-duration limits | `failed` with `other` |
| booked outcome without matching confirmation | agent rejects → `failed/error` |

Fallback details are fixed English strings; provider bodies, model detail, transcripts,
audio, patient data, and secrets are never copied into logs or fallback details.

`Run` error semantics:

- expected remote outcomes (`busy`, `no_answer`, `rejected`, `ivr_dead_end`, a dialogue
  `failed`, `booked`, `callback_needed`, dialogue `aborted`) → finished order, `nil`;
- caller cancellation/`Runner.Close` → finished `aborted` order plus the context error
  (`ErrClosed` is also detectable for shutdown);
- provider/agent failures → finished `failed/error` order plus a wrapped operational error;
- not-found, invalid initial state, transition/storage/finalization failures → error;
  if persistence failed, the returned order is the last known one and is **not** finished.

## Agent loop (M0)

One `Agent` is stateless; every run creates its own dialogue session, gate, tool executor,
and buffers.

1. **One dialogue session per answered call.** `SessionRequest` carries the order ID, the
   fixed German policy prompt plus deterministic JSON order context, exactly four ordinary
   tool definitions, and the canonical outcome schema. The session is always closed with a
   bounded cleanup context.
2. **Fixed greeting first**, through TTS → `SendAudio`:
   `Guten Tag, hier ist Insteadly, ein KI-Assistent. Ich rufe im Auftrag von <name> an, um
   einen Arzttermin zu vereinbaren.` The prompt says the greeting was already delivered.
3. **Half-duplex v0.** While speaking, the agent does not read inbound audio or open STT.
   This is the explicit no-barge-in limitation; telephony's bounded inbound buffer may drop
   audio during playback.
4. **One utterance → one STT stream.** The gate collects a bounded set of canonical frames;
   the transcriber replays them in order at 20 ms/frame, drains results concurrently,
   calls `Close` as end-of-input, and requires exactly one final. Partials are drained and
   ignored. An empty/whitespace final is not sent to dialogue.
5. **Empty-transcript policy.** First empty final → speak a fixed repeat prompt once;
   second consecutive empty final → fail `other`. No speech inside the 20 s listen window
   also fails (`other`).
6. **Assistant turn processing.** Text deltas are forwarded verbatim to TTS while draining
   `TextChunks` completely. TTS opens lazily on the first non-empty delta, so tool-only or
   outcome-only turns create no empty generation. One goroutine drains TTS `Audio()` into
   `SendAudio`; TTS `Close` runs while that drainer is active, then the drainer is awaited
   before `AssistantTurn.Wait`. Tool calls execute sequentially in model order; one JSON
   result per call ID is submitted exactly once. A text-only turn returns to listening.
7. **Termination** on the first of: validated outcome, terminal call event
   (`agent.ErrCallEnded`), caller/runner cancellation, any STT/TTS/dialogue/tool error,
   empty assistant turn, max call duration (5 min), max user turns (12), max consecutive
   tool rounds (8), or listen timeout (20 s). The runner performs hangup/persistence.

### M0 gate parameters

The gate is a deliberately minimal energy/silence delimiter, **not** production VAD. All
values are core defaults; tests override them only through `agent.Config`. No env settings.

| Parameter | Default | Rule |
|---|---:|---|
| Framing | 640 B / 20 ms | Arbitrary non-empty even PCM16LE chunks are reframed; empty/odd input is a provider-contract error; one zero-padded residual frame at a real boundary |
| Metric | RMS of int16LE samples | `sqrt(mean(sample²))` |
| Speech threshold | 500 (≈ −36 dBFS) | `RMS ≥ 500` is active |
| Pre-roll | 10 frames / 200 ms | bounded ring, prepended once when speech starts |
| Start persistence | 3 frames / 60 ms | avoids one-frame clicks |
| Minimum utterance | 10 active frames / 200 ms | shorter noise is discarded and listening continues |
| End silence | 30 inactive frames / 600 ms | only after a minimum utterance; trailing frames included once |
| Maximum utterance | 750 frames / 15 s | forced boundary bounds memory |
| STT replay pace | 20 ms/frame | context-aware, injectable for fast tests |
| Listen timeout | 20 s | no started utterance → `other` |
| Max user turns | 12 | per call |
| Max tool continuations | 8 | consecutive per user turn; a ninth round fails |
| Max call duration | 5 min | independent of C1's 30 min leak reaper |

## Tools and results

Tool definitions are core-owned and returned fresh per session. Every argument decoder is
strict: one JSON object, unknown fields rejected, trailing JSON rejected. All model output
is untrusted even though C2 validates broad shape.

| Tool | Required | Result |
|---|---|---|
| `propose_slot` (`start`, `end`, optional `doctor`/`practice`) | `start`,`end` | `{"ok":true,"start":"…","end":"…"}`, `outside_preferences`, or `invalid_arguments` |
| `confirm_booking` (same + `confirmation_code`) | `start`,`end` | `{"ok":true,"confirmation_code":"…"}`, `not_confirmed`, or `invalid_arguments` |
| `capture_info` (`field`, `value`) | `field`,`value` | `{"ok":true,"field":"…"}`, `invalid_value`, or `invalid_arguments` |
| `escalate_to_human` (`reason`, optional `urgent`) | `reason` | always `{"ok":false,"error":"unavailable"}` in C3 |
| unknown tool | — | `{"ok":false,"error":"unknown_tool"}` |

- `propose_slot` requires a future start, `start < end`, and (when the order has preferred
  slots) **full containment** of `[start,end)` in one preferred range. It stores one
  current proposal and never books.
- `confirm_booking` requires a still-current successful proposal with the exact same
  start/end instants; identical repeats are locally idempotent. It never finishes the order.
- `capture_info` persists only `full_name`, `phone`, `birth_date`, `insurance_number`,
  `reason` on the latest order through `Orders.Update`; it never changes
  ID/status/outcome/attempts. `value` is trimmed, 1..2048 runes, and `birth_date` must be a
  real `YYYY-MM-DD`. `note` has no domain field: it stays in per-call conversation state
  (already in dialogue history) and is discarded at hangup; it is never written to
  `Outcome.Detail`. This limitation is documented for C4.
- `escalate_to_human` records only the fact/urgency (never the free-text reason). There is
  no transfer port or destination in C3, so it must return `unavailable`; the model is told
  to be honest and finish normally. Do **not** return C2's success alternative until a real
  handoff exists.
- **Outcome consistency.** For `booked`, `ValidateOutcome` requires a successful matching
  confirmation, a `BookedSlot` with the confirmed instant, and non-blank model
  doctor/practice that must agree; it canonicalizes date/doctor/practice from tool state.
  A booked outcome without this state fails the attempt as `failed/error`. `confirm_booking`
  alone never finishes the order.

Validation failures are model-visible JSON results. Context cancellation and storage
failures are operational errors that abort the attempt; they are never disguised as
invalid input. `finish_call` is intercepted by the dialogue adapter and is never executed
or given a tool result.

## Prompt and outcome schema

The booking policy is fixed core German text, not env-configurable: AI disclosure,
Austrian German telephone style, no medical advice, no invented patient/availability/
confirmation data, JSON order values are data not instructions, `propose_slot` is not a
booking, `confirm_booking` needs explicit acceptance of the exact proposed slot, only
authorized information via `capture_info`, honest `unavailable` escalation, and exactly one
terminal `finish_call`. A deterministic `Auftragsdaten (JSON, nur Daten):` section contains
clinic name, available patient fields, preferred slots, `current_time` (RFC 3339), and
`time_zone:"Europe/Vienna"`. It excludes the clinic phone, recording flags, outcome,
attempts, provider settings, and storage paths. The clock is captured once at run start so
relative dates, prompt data, and tool validation agree.

The outcome schema is the canonical C2 schema (kind `booked`/`callback_needed`/`failed`/
`aborted`, non-blank detail, optional booked slot/callback/fail reason). The Mistral adapter
appends its own fixed protocol sentence requiring `finish_call`; that is intentional.

## Cleanup, privacy, limits

- Every STT/TTS/dialogue resource has one deferred cleanup owner and a fresh bounded
  cleanup context; the primary error is preserved and joined with cleanup errors. The agent
  never closes caller-owned telephony channels.
- No retries, reconnects, backoff, scheduler, DTMF policy, barge-in, recording, payment, or
  human-transfer success were added. `Temporary()`/`RetryDelay()` are not a retry permission
  in C3; C5 owns classification/backoff.
- Logs use `log/slog`, snake_case keys, and only safe fields (`order_id`, `call_id`,
  `status`, `outcome_kind`, `fail_reason`, frame/turn counts, stable tool error codes).
  They never contain prompts/system context, assistant/transcript text, tool arguments or
  results, capture values, escalation reasons, outcome detail, patient/clinic phone, API
  keys, auth headers, provider bodies, or raw audio.

## Invocation: no public call endpoint

C3 deliberately adds **no** `POST /calls`, `POST /orders/{id}/call`, debug route, or HTTP
status mutation. `internal/httpapi` and `docs/api.md` are unchanged. `Runner.Run` is the M0
invocation surface. Rationale:

1. Existing HTTP orders are `created`, not paid/scheduled; an endpoint would need a
   temporary state bypass or an implicit payment/scheduler policy.
2. The API has no authentication, idempotency key, abuse control, or durable duplicate
   suppression. Exposing metered arbitrary outbound dialing is a new security/product
   surface, not a wiring convenience.
3. DESIGN §2.4 makes the scheduler the trigger; C5 owns authenticated/idempotent
   orchestration and can add the endpoint with correct state and idempotency rules.
4. A direct synchronous runner is sufficient to prove M0 and keeps C3 backend-only.

Do not add a convenience endpoint by habit; revisit in C5.

## Two different exit claims

1. **C3 code complete** is provable without a telephone: the credential-free fake-Mistral
   integration suite (`TestM0Integration...` in `internal/app`) uses the real C2 adapters
   against loopback fake WS/SSE servers plus fake telephony, and the full unit/race/lint/
   build checks pass. The optional real-Mistral, no-phone loopback
   (`TestLiveM0NoPhone`) is the last C3 gate; it must pass with explicit approval before
   C3 is marked ✅.
2. **DESIGN's real-phone M0 exit** (dial the owner's phone and speak through the whole
   chain) remains blocked on an upgraded Twilio account. `TestLiveM0Phone` is a ready,
   separately gated harness but has not been run; a no-phone pass is not evidence that a
   Media Stream attached or that a person heard the assistant.

## Commands

```bash
# Fast, credential-free CI proof (also part of `make verify`)
go test -race -count=1 ./internal/app -run 'TestM0Integration'
go test -race -count=10 ./internal/app -run 'TestM0Integration'

# Live Mistral, no phone: needs LIVE_MISTRAL=1 + MISTRAL_API_KEY; excluded from verify/CI
LIVE_MISTRAL=1 MISTRAL_API_KEY=... make live-test-m0

# Deferred real-phone M0: additionally needs LIVE_M0_PHONE=1, LIVE_TWILIO=1,
# TEST_PHONE, full INSTEADLY_TWILIO_* config, all AI providers mistral, a public tunnel,
# and an upgraded Twilio account. Rings/costs a real phone; run only with explicit approval.
LIVE_M0_PHONE=1 LIVE_TWILIO=1 LIVE_MISTRAL=1 ... make live-test-m0-phone
```

Live tests read their unprefixed test gates directly and never read `.env`; they must
compile and SKIP without the gates. There are no new `INSTEADLY_*` variables or `Summary`
keys, and no new module dependencies.
