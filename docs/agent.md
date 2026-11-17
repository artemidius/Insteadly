# Call runner + conversation engine (C4)

C4 replaces C3's bounded M0 utterance gate with a streaming, provider-neutral conversation
engine: continuous 20 ms audio processing, core VAD and end-of-turn, full-duplex barge-in,
provider playback marks, deterministic IVR navigation, conservative hold detection, and a
bounded structured-outcome repair path. C3's booking/tool invariants and runner-owned
exactly-once finalization are unchanged.

```
internal/core/runner   one order attempt: claim, dial, state machine, hangup, final outcome
internal/core/agent    streaming conversation: VAD, STT workers, barge/hold/IVR, recovery
```

Both import only `internal/core/{ports,domain}` (plus stdlib/`log/slog`; the runner also
imports the agent). `internal/app` is the composition root. No adapter imports the
runner/agent, and there is still no public HTTP place-a-call endpoint.

## Ownership

| Concern | Owner | Does not own |
|---|---|---|
| `Orders.Get`, `scheduled → dialing`, `Attempts++`, one `Dial` | runner | scheduling, retries, payment |
| Consuming `CallEvent`, order transitions, answered/ended signaling | runner | STT/TTS/dialogue policy |
| Bounded post-answer DTMF/playback-mark relay | runner | digit choice, confirmed history |
| Final outcome selection, exactly one `Finish`, best-effort `Hangup` | runner | model parsing |
| Streaming VAD/end-of-turn, one STT stream per utterance | agent | dialing, order transitions, Hangup |
| Barge-in, playback generations, confirmed assistant history | agent | provider wire details |
| Deterministic IVR policy, hold policy, outcome recovery | agent | real transfer, scheduling |
| Tool schemas/validation/execution, per-call booking state | agent | `finish_call` (adapter-intercepted) |
| Provider protocols, µ-law conversion, `clear`/`mark` wire messages | Twilio adapter | any core policy |
| Interrupt/control history reconciliation | Mistral dialogue adapter | core wording/limits |
| Non-flushing TTS abort | Mistral TTS adapters | playback policy |
| Construction/DI | `internal/app` | conversation/business rules |

The agent's narrow `AudioSession` can read inbound audio, send audio/DTMF, clear queued
audio, and place playback marks. It still cannot consume the runner's event channel or hang
up. `Runner` remains the only consumer of `ports.CallSession.Events`.

## Runner sequence and control relay

```
caller (test/future scheduler)   runner                        telephony / agent
        │ Run(order_id)           │                              │
        │────────────────────────►│ Orders.Get/claim, Dial ─────►│
        │                         │◄────────────── CallSession    │
        │                         │ events → statuses, StreamReady │
        │                         │ Agent.Run(..., ended, control)►│
        │                         │◄──────── outcome or error ────│
        │                         │ Hangup, drain end, Finish     │
        │◄──────── final order ───│                              │
```

- **Terminal signal stays separate.** `ended` is a capacity-one guaranteed signal; `EventEnded`
  is mirrored there and the relay keeps draining until the adapter closes the channel.
- **Control relay is bounded and ordered.** Post-answer `EventDTMF` and `EventPlaybackMark`
  go to a capacity-64 control channel in wire order. A full buffer records an overflow,
  closes `control`, and fails the attempt — even if the agent races to an otherwise valid
  outcome, the runner refuses to persist it. Digits/marks are never silently dropped,
  reordered, or placed behind a terminal event.
- Pre-answer `EventDTMF` remains debug-logged; a pre-answer `EventPlaybackMark` is a
  provider-contract error (no core segment can have produced it).
- Exactly one `Finish`, exactly one `Hangup`, one `Attempts++` per `Run`; agent outcomes
  (including a direct `failed/ivr_dead_end`) always win over the runner's local hangup
  reason. Busy/no-answer/rejected/aborted/error fallbacks are unchanged from C3.

## Conversation state machine

One per-call state machine owns listening/utterance/assistant/tools/holding/IVR/terminal.
Independent goroutines never decide whether the user is speaking.

| State | Inbound policy | Boundary |
|---|---|---|
| `listening` | noise-adaptive VAD; no assistant audio | 60 ms onset opens one STT worker; listen timeout 20 s |
| `utterance` | pre-roll + live frames feed STT; results drained concurrently | 700 ms hangover after ≥200 ms speech closes input; 15 s forced boundary |
| `assistant` | full-duplex VAD and playback-mark reconciliation | 60 ms stronger onset barges in; cancel model/TTS, clear playback |
| `tools` | capture/transcribe one bounded pending utterance; never cancel an atomic tool | tool results are submitted before a new assistant can speak |
| `holding` | detect speech/silence/DTMF; never STT hold audio | speech resumes an utterance; 500 ms silence returns to listening; 90 s → `failed/other` |
| `ivr_wait` | wait for a new menu, hold, or human speech after a digit | repeats/ambiguity/loops/4-step/15 s bounds follow the policy |
| `terminal` | no new STT/TTS/dialogue work | workers canceled and joined; runner persists one outcome |

## Streaming VAD and end-of-turn

All DSP is stdlib-only (`math`, `encoding/binary`, `sort`-free fixed insertion sort): PCM16
decode → DC-removed normalized RMS/ZCR → Hann-windowed Goertzel powers on a 39-bin 100 Hz
grid plus 350/440/480/620 Hz hold probes → normalized positive spectral flux → spectral
flatness → strong-tone test. Only the previous 39-bin magnitude vector, a 15-frame pre-roll
ring, and a 150-frame hold slate are retained; no recording or spectrogram is stored.

| Parameter | Default | Rule |
|---|---:|---|
| Frame | 640 B / 20 ms | arbitrary even chunks reframed with residual bytes; empty/odd input is a contract error |
| Noise floor | 0.003, clamp [0.001, 0.05] | adapts only on non-speech, non-hold frames (up 0.02 / down 0.005); frozen during speech, assistant playback, and hold |
| Normal energy threshold | `clamp(3·noise, 0.012, 0.10)` | onset additionally needs flatness `< 0.75`, flux `≥ 0.04`, no strong tone |
| Barge threshold | `max(normal, 4·noise, 0.024)` | 3 consecutive barge frames / 60 ms; energy/flatness/tone only (no per-frame flux) |
| Start | 3 onset frames / 60 ms | pre-roll ring prepended exactly once |
| Pre-roll | 15 frames / 300 ms | bounded; never replayed from a whole-utterance buffer |
| Minimum utterance | 10 active frames / 200 ms | shorter candidates are discarded, listening continues |
| End silence | 35 frames / 700 ms | applies only after minimum active speech |
| Forced boundary | 750 frames / 15 s | continuing speech may open the next utterance from recent pre-roll |
| Listen timeout | 20 s | no utterance → `other` limit |
| Noisy state | — | no new env knobs; thresholds are reviewable core constants |

Each utterance gets exactly one STT stream, opened at confirmed onset. The pump bursts the
bounded pre-roll into a capacity-64 worker queue, then forwards live frames as they arrive;
the worker drains cumulative partials concurrently and treats `Close` as end-of-input. Only
exactly one final drives the conversation; partials, early finals, and empty audio never do.
At most two live workers exist (one active plus one closing) and one final result can be
pending; overflow is an explicit conversation/provider error, never drop-oldest audio
presented as a complete transcript.

## Playback evidence, text segmentation, and barge-in

C3's single TTS stream could not say which generated words were played. C4 splits streamed
assistant text into bounded segments at `.?!`/newline, preferring a comma/semicolon/space
before a hard split at 240 Unicode code points. Each segment gets its own `SpeechStream`,
exact segment text, and an opaque core-generated marker. After the last `SendAudio` the
speaker queues `MarkPlayback` and waits for the normalized provider mark. A segment is
confirmed only after at least one nonempty canonical frame was actually forwarded; a
zero-audio provider result is a playback error that confirms no text and cannot unlock tools
or an outcome. At most one segment awaits a mark; a 30 s ack timeout is a playback error.

**A mark means provider playback reached that point — never that a person heard or
understood it.** Only complete marked segments enter confirmed dialogue history; an
unmarked partially spoken segment is omitted and the model is told the answer was cut off.

Barge-in sequence on confirmed assistant-state onset:

1. Publish barge immediately (before any STT handshake), preserving the utterance pre-roll.
2. Cancel the per-assistant-turn context (stops the Mistral request) and start
   `SpeechStream.Abort` with an independent 2 s context; keep draining only until `Audio`
   closes, discarding late/old-generation chunks.
3. Concurrently `ClearAudio` with its own 2 s context. Twilio increments a playback epoch,
   purges queued media, resets residual encoder bytes, and sends the wire `clear`; a clear
   failure fails the attempt rather than speaking over stale audio.
4. Join abort/clear, snapshot complete marks observed before clear, wait for the canceled
   dialogue turn's rollback, then call `DialogueSession.Interrupt(confirmedText)`. Prior
   user/tool history is preserved; unexecuted calls/outcome are dropped; no tool re-runs.
5. The barging utterance's single final is processed as the next real user turn (empty
   audio → the existing bounded repeat behavior; no synthetic user text).

Hold detection (`150`-frame / 3 s slate) distinguishes silence from tone holds (same strong
probe in ≥120/150 frames, or a cadence requiring the same dominant probe, at least two similar
on-bursts, and an internal off gap) and music-like audio (≥90% energy, <20%
speech-classified, low spectral variation, ≥2 stable peaks). On confirmed hold: clear
playback, never open STT for hold audio, never send hold text to dialogue, keep processing
DTMF/terminal events. Speech exits with pre-roll; 500 ms silence returns to listening; 90 s
continuous hold is `failed/other` detail `hold timeout`. It is never `ivr_dead_end` or a
fabricated callback.

The greeting is split into an AI-disclosure segment and a purpose segment, each marked. If
either is cleared/unconfirmed, the lazily created dialogue session receives a prompt
variant that requires the missing disclosure/purpose in the first real response. C4 never
claims the full greeting was spoken after a clear.

## IVR navigation (evidence-gated)

Only final transcripts are parsed; partials never press digits. The deterministic parser
normalizes German/English text and number words, then requires an explicit menu imperative
adjacent to a **single** digit/`*`/`#` and an option purpose. Digits in dates, phone numbers,
office hours, or ordinary speech are not options.

- The imperative and the digit's purpose must be scoped to the **same option**: hard
  punctuation separates clauses, and within a coordinated clause each digit owns a
  nearest-digit purpose segment. A purpose in a different clause (for example "Press 1 for
  office hours; for appointments, stay on the line") cannot justify pressing that digit.
- Appointment/reception targets (`termin`, `termine`, `terminvereinbarung`, `terminvergabe`,
  `anmeldung`, `rezeption`, `ordination`, `appointment`, `reception`, …) are sent only when
  exactly one target digit is present in the **current** menu; ambiguous or
  unrelated/emergency-only menus press nothing.
- One `SendDTMF` per menu evidence; the menu is cleared after success. The port accepting a
  digit is not proof the menu advanced.
- Human speech without menu syntax exits IVR and goes through normal dialogue; an IVR-only
  dead end never creates a dialogue session or a booking.
- `failed/ivr_dead_end` (fixed detail) is returned directly for repeated/looping menus, a
  menu with no unique target after its second presentation, more than 4 steps, and 15 s of
  no menu/hold/human activity after a digit. A `SendDTMF` operational error is
  `failed/error`, not a dead end.
- Inbound `EventDTMF` is bounded evidence (16-event ring) only and is never echoed back.

The agent's direct `ivr_dead_end` outcome is persisted by the runner; the runner's later
local `EndAborted` must not replace it. The real outbound-DTMF path remains unproven on the
Twilio trial and is blocked for real-phone M1.

## Bounded structured-outcome repair

The shared turn runner always drains text, reconciles playback, awaits the result, executes
ordinary tools at most once, and applies `ToolExecutor.ValidateOutcome` before returning any
outcome. Only these model/conversation failures are repaired via `SendControl` with fixed
German system-role instructions:

| Trigger | Intent |
|---|---|
| `domain.ErrInvalidOutcome` from reserved `finish_call` | re-emit a schema-valid outcome |
| `ValidateOutcome` rejection | make the outcome match actually executed proposal/confirmation state; never invent a booking |
| empty assistant turn | reply usefully, or finalize with one valid `finish_call` |
| agent-owned 45 s assistant-turn deadline with the call context alive | resume without repeating tool effects |

- **At most two recovery controls per call, not per user turn.** The initial bad turn does
  not count; a third need returns `errOutcomeRecoveryExhausted` as an `agent.LimitError`
  (`failed/other`). A later valid user turn does not reset the budget.
- Recovery cannot bypass `MaxToolRounds=8`, duplicate a tool ID, or mutate/clear the booking
  executor's proposal, confirmation, or captures. `booked` still requires the exact C3
  confirmation. Transport/STT/TTS/clear/mark/caller-cancel/terminal failures are
  operational or terminal, never repair prompts.
- Control instructions/results never contain provider bodies, model prose, validation
  strings, transcripts, patient values, or credentials.

`escalate_to_human` remains `{"ok":false,"error":"unavailable"}`. C4 implements **no real
transfer**, and the prompt continues to say no handoff occurred.

## Ports and domain changes (C4)

- `domain.EventPlaybackMark` plus `domain.CallEvent.PlaybackID` (opaque, ≤64 ASCII chars,
  echoes `MarkPlayback`; never text/patient data/provider SIDs/secrets).
- `ports.CallSession.ClearAudio(ctx)` and `MarkPlayback(ctx, playbackID)` — required,
  provider-neutral, ordered with `SendAudio`; every implementation and fake updated.
- `ports.DialogueSession.Interrupt(ctx, confirmedText)` and `SendControl(ctx, instruction)`.
- `ports.SpeechStream.Abort(ctx)` — non-flushing barge path; normal `Close` still flushes.
- No other port/domain change; no new module dependency. `go.mod`/`go.sum` remain
  `github.com/coder/websocket v1.8.15`.
- `internal/core/config`, `Summary()`, `internal/httpapi`, and `docs/api.md` are unchanged;
  there are **no new `INSTEADLY_*` names**. VAD/hold/recovery tuning is via `agent.Config`
  only (tests), never env.

## Cleanup, privacy, limits

- Every STT/TTS/dialogue worker has one owner, bounded queues, and a joined cleanup. On
  exit the agent stops/cancels its audio workers first, then closes the dialogue session;
  each phase receives a fresh bounded cleanup budget created at exit (not a context created
  at Run entry, which would already be expired after a long call). The runner shuts the
  control relay down afterwards. No channel is closed by both producer and consumer. STT
  end-of-input is ordered behind the bounded feed queue, so accepted audio is always drained
  before `Close`.
- Cleanup timeout is **35 s** so per-segment closes exceed C2's 15 s STT and 30 s realtime
  TTS final-drain limits. The interruption bound is a separate **2 s**; the runner's 5 s
  hangup/finalization timeout is unchanged.
- Maximum call duration remains 5 min and is never extended by VAD, hold, IVR, or recovery.
  Other limits: 20 s listen, 12 user turns, 8 tool continuations, 2 recovery controls, 4 IVR
  steps, 15 s IVR wait, 90 s continuous hold, 30 s playback ack, 45 s assistant turn.
- Logs use safe categories only: order/call IDs, status/outcome kind, fail reason codes,
  counts, and stable control reason codes. Never prompts, transcripts, model prose, tool
  args/results, captured patient values, menu text, audio, keys, or provider bodies. No
  recording, raw-audio, partial-transcript, or menu storage is added.

## Twilio adapter additions

`internal/adapters/telephony/twilio` adds outbound `{event:"clear",streamSid}` and
`{event:"mark",streamSid,mark:{name}}` on the existing bidirectional stream:

- one writer goroutine owns media/mark/clear order; a per-stream write serializer keeps wire
  messages atomic;
- `ClearAudio` increments a playback epoch, purges queued actions, resets residual encoder
  bytes under the writer, and sends `clear`; media accepted before the clear is dropped even
  if it was already dequeued;
- with no attached stream, clearing local playback succeeds (an old disconnected buffer
  cannot outlive its stream); after call end the established safe-end semantics apply;
- inbound `mark` becomes exactly one `EventPlaybackMark` with the opaque ID; empty/oversized
  marks are debug-logged and ignored; late marks from cleared generations are valid races
  ignored by ID in core;
- inbound DTMF, µ-law conversion, `StreamReady`, reconnect, and `stop`-is-not-call-end are
  unchanged.

## Mistral adapter additions

- TTS realtime/offline implement non-flushing `Abort`: cancel the WebSocket/HTTP request,
  discard queued text, close `Audio`, and never emit `input_text.end` / an offline flush.
  Normal `Close` flush semantics are unchanged. Abort/Close/FeedText are serialized so no
  send/close panics.
- Dialogue `Interrupt(confirmedText)` rolls history back to the triggering user/tool
  messages, keeps only confirmed assistant speech, clears unexecuted pending calls/outcome,
  and reopens a `finish_call`-completed session. `SendControl(instruction)` starts a turn
  from a system-role control message (never user speech), preserves the failed/stalled
  candidate for retry, and rolls back a rejected finish_call.
- The reserved `finish_call`, tool IDs, outcome schema, and one-in-flight-turn rule are
  unchanged.

## Fake, real-Mistral, and real-phone evidence

1. **C4 code complete (credential-free):** unit/race suites for DSP/VAD, listeners, speaker,
   barge, IVR, hold, recovery, Twilio clear/mark/epoch, Mistral interrupt/control/abort, and
   the loopback fake-Mistral/fake-telephony chain in `internal/app`. `TestLiveM1NoPhone` is
   the last C4 gate and must be run separately before C4 is marked ✅.
2. **DESIGN real-phone M1 remains blocked:** Twilio Media Streams on the trial account and a
   controlled real IVR are unavailable. No no-phone test proves audible stream audio,
   natural end-of-turn for a person, stopped playback on a real barge, an IVR accepting a
   digit, or hold music not being transcribed. `TestLiveM1Phone` is separately gated and
   must never ring without every gate, a controlled destination, and explicit approval. The
   earlier real-phone M0 blocker is also still open.

## Commands

```bash
# Credential-free full-chain proof (also part of `make verify`)
go test -race -count=1 ./internal/app -run 'TestM0Integration'
go test -race -count=10 ./internal/app -run 'TestM1Integration'

# Compile the live harnesses and confirm they SKIP without gates
go test -tags live -run 'TestLiveM1(NoPhone|Phone)$' ./internal/app

# Real Mistral, no phone: LIVE_MISTRAL=1 + MISTRAL_API_KEY; excluded from verify/CI
LIVE_MISTRAL=1 MISTRAL_API_KEY=... make live-test-m1

# Deferred real-phone M1: LIVE_M1_PHONE=1 LIVE_TWILIO=1 LIVE_MISTRAL=1,
# MISTRAL_API_KEY, TEST_PHONE, full INSTEADLY_TWILIO_* + public tunnel, upgraded account,
# controlled destination. Rings/costs a real phone; never run without explicit approval.
LIVE_M1_PHONE=1 LIVE_TWILIO=1 LIVE_MISTRAL=1 ... make live-test-m1-phone
```

The no-phone harness reads its unprefixed gates directly and never reads `.env` or Twilio
configuration. The phone harness calls `config.Load` (possibly reading `.env`) only after
every gate passes. Both compile and SKIP without gates. **No live test was run by the C4
implementation session; the conductor runs the no-phone gate.**
