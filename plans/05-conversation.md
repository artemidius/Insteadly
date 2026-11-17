# Chunk C4 — Conversation (M1: VAD, barge-in, IVR, hold, outcome repair)

> **Status:** implemented on `feat/c4-conversation` (PR #6); 🔵 code complete pending the
> gated real-Mistral no-phone live test (§7.3). The C4 implementation session ran **no live
> test**; see §13 for its dated notes. Chunk owner: conductor for the no-phone gate.
> **Source of truth:** [DESIGN.md](../DESIGN.md) — this plan derives from it and must not
> contradict it. If a conflict appears, stop and resolve it against DESIGN.md, not silently.
> **Progress register:** [PROGRESS.md](../PROGRESS.md) — update it only in the session that
> implements this chunk, using its update protocol.
> **Previous chunks:** [C0](01-foundation.md) ✅; [C1](02-twilio-telephony.md) code complete,
> with live Media Streams blocked by the Twilio trial and outbound DTMF's only live attempt
> returning HTTP 400; [C2](03-mistral-ai.md) ✅; [C3](04-m0-e2e.md) ✅ code complete, including
> the real-Mistral no-phone gate. Planning baseline: `main` = `fbc2d40`.
>
> **Two different exit claims — do not conflate them:**
>
> 1. **C4 code complete** is provable without a telephone: deterministic core DSP/policy tests,
>    race/lint/build checks, fake telephony with scripted audio/DTMF/playback/hold, the real C2
>    Mistral adapters against loopback fake WS/SSE servers, and an explicitly gated real-Mistral
>    no-phone test must pass.
> 2. **DESIGN real-phone M1** remains blocked until Twilio Media Streams and a controlled real
>    IVR can actually be exercised. A no-phone pass proves neither that Twilio played/cleared
>    audio, nor that a person was heard, nor that an IVR accepted a digit. It also does not
>    retroactively satisfy the still-blocked real-phone M0 exit.

---

## 1. What this chunk is

C4 replaces C3's bounded, buffered M0 utterance gate with a **streaming, provider-neutral
conversation engine**. The core consumes canonical inbound PCM16 continuously, detects speech
and end-of-turn itself, streams each delimited utterance into its own STT session, accepts an
interruption while the assistant is generating or speaking, cancels synthesis, clears queued
telephony playback, and reconciles only provider-confirmed assistant audio with dialogue history.

It also adds deterministic IVR navigation over the existing `SendDTMF` port, conservative hold
music/tone detection, and a bounded recovery path for invalid, missing, or stalled structured
outcomes. C3's booking/tool invariants and runner-owned exactly-once finalization remain intact.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | Streaming 20 ms audio pump, noise-adaptive VAD, speech onset, hangover/end-of-turn, pre-roll, forced utterance bound | `internal/core/agent/vad.go`, `audio_features.go` |
| 2 | One STT stream per utterance, with audio fed while speech is arriving and cumulative partials drained concurrently | `internal/core/agent/listen.go`, `transcribe.go` |
| 3 | Full-duplex barge-in while an assistant turn is active; cancel the turn/TTS, clear queued playback, preserve the new user utterance | `internal/core/agent/bargein.go`, `speak.go` |
| 4 | Ordered playback markers and conservative reconciliation of confirmed-played assistant segments with dialogue history | core agent/runner, ports/domain, C1/C2/stub adapters |
| 5 | Provider-neutral `ClearAudio` and `MarkPlayback`; Twilio `clear`/`mark` wire messages with stale-generation protection | `internal/core/ports`, `internal/adapters/telephony/twilio` |
| 6 | Explicit dialogue interruption and core-control operations; Mistral history rollback/commit rules | `internal/core/ports/dialogue.go`, `internal/adapters/dialogue/mistral` |
| 7 | Deterministic German/English menu parser and bounded IVR navigation policy; real outbound digits only on transcript evidence | `internal/core/agent/ivr.go` |
| 8 | Conservative hold tone/music detection using stdlib DSP; wait/exit/timeout policy | `internal/core/agent/hold.go` |
| 9 | At most two per-call outcome/empty-turn/model-stall recovery controls, preserving C3 tool/outcome consistency | `internal/core/agent/recovery.go`, `agent.go` |
| 10 | Runner relay for post-answer DTMF and playback marks while retaining a separate guaranteed terminal signal | `internal/core/runner/events.go` |
| 11 | Credential-free scripted fake-telephony and loopback fake-Mistral integration coverage for all policies | core/adapter tests, `internal/app/m1_integration_test.go` |
| 12 | Optional real-Mistral no-phone and separately gated real-phone M1 harnesses | `internal/app/m1_live_test.go`, `m1_phone_live_test.go` |
| 13 | C4 architecture/operator documentation and implementation-session progress update | `docs/agent.md`, `docs/architecture.md`, `README.md`, `PROGRESS.md` |

### Out of scope (do NOT implement)

- **Acoustic echo cancellation, speaker diarization, source separation, neural/learned VAD, or a
  word-level text/audio aligner.** M1 uses DSP heuristics and whole-segment playback evidence.
  Echo-triggered false barge-in remains an explicit real-phone risk, not something tests may hide.
- **Mistral server-side turn detection.** It stays disabled; a partial/final transcript never
  overrides core end-of-turn ownership (DESIGN §1.6).
- **Decoding DTMF from audio.** C1 already supplies normalized `EventDTMF`; do not create a second
  detector that can double-count provider events.
- **Automatic redial/retry/scheduling, a public place-a-call endpoint, recording/transcript storage,
  metrics dependencies, payments, notifications, or web UI.** These belong to C5+.
- **Real human transfer.** `escalate_to_human` remains honestly `unavailable`; see §3.12.
- **An in-band DTMF fallback or a new telephony provider.** First obtain real evidence about the
  existing C1 `<Play digits>` path after the Twilio upgrade.
- **Exact claims about partially played text.** A Twilio mark means provider playback progressed,
  not that a human heard or understood it. Unmarked partial segments are never rewritten as though
  their full text was spoken.
- **New module dependencies or production tuning env vars.** See §5.

---

## 2. Preconditions and baseline

1. Start on `main` = `fbc2d40` or a later commit explicitly containing C3. Read DESIGN §1.6,
   §2.1.3, §2.2, §2.4, §5, and §6; C3 plan §11 is the direct scope contract.
2. Preserve the dependency law and canonical audio format: signed PCM16 little-endian, mono,
   16 kHz, 320 samples/640 bytes per 20 ms frame.
3. Preserve the module freeze:
   ```bash
   go list -m all
   ```
   must list only the main module and `github.com/coder/websocket v1.8.15`.
4. Re-read C3's actual behavior before editing: one buffered utterance per STT stream; STT server
   turn detection disabled; partials are cumulative; TTS streams while dialogue text arrives;
   `finish_call` is intercepted by the dialogue adapter; `ToolExecutor.ValidateOutcome` guards
   booked/tool consistency; the runner alone finalizes orders and hangs up.
5. Re-read C1's actual behavior: outbound `SendDTMF` exists and uses a call update with
   `<Play digits>`; inbound Media Stream DTMF exists; outbound `clear`/`mark` do **not** exist yet;
   inbound `mark` is only logged. `StreamReady` is a once-only initial-readiness hook, not proof
   of every later DTMF reconnect.
6. **Observed planning baseline (2026-11-17):** the first `go test -count=1 ./...` could not finish
   because the workspace sandbox denied writes under `~/Library/Caches/go-build`; this was an
   environment failure, not a test failure. The rerun
   ```bash
   GOCACHE=/tmp/insteadly-c4-gocache go test -count=1 ./...
   ```
   **passed every package** at `fbc2d40`. Use a writable `/tmp` GOCACHE/GOMODCACHE when needed.
7. `/usr/bin/make` is broken on this machine. Use
   `/Library/Developer/CommandLineTools/usr/bin/make`; local `golangci-lint` is optional, while CI
   uses v2.15.0.
8. Do not read or print secrets merely to run fake tests. Live tests compile and skip without their
   explicit gates and credentials. No live telephone test runs in CI or `make verify`.

---

## 3. Ports → components and exact contracts

### 3.1 Ownership map

| Port/domain contract | C4 consumer/component | Adapter work |
|---|---|---|
| `CallSession.Inbound` / `AudioChunk` | continuous audio pump → framing → features → VAD/hold | C1 remains responsible for µ-law/rate conversion and bounded input |
| `CallSession.SendAudio` | segment speaker, one canonical chunk at a time | Twilio retains ordered bounded output; stale-generation chunks are dropped |
| **new** `CallSession.ClearAudio` | barge-in and confirmed hold entry | Twilio sends `clear`; stub/fakes model local + remote queue removal |
| **new** `CallSession.MarkPlayback` | speaker places a marker after each known text segment | Twilio sends `mark`; stub/fakes return a normalized mark event |
| existing `CallSession.SendDTMF` | deterministic IVR policy only | C1 implementation stays behind the port; live gate is revisited, not assumed |
| existing `CallSession.Events` | runner remains sole telephony consumer; relays DTMF/marks to agent | Twilio translates inbound `mark` into `EventPlaybackMark` |
| `SpeechToText.TranscribeStream` | one worker per VAD-delimited utterance | C2 protocol unchanged; no server VAD |
| `TextToSpeech.Speak` / **new** `SpeechStream.Abort` | one generation per bounded assistant text segment; immediate barge-in abort | Realtime cancels WS generation; offline cancels any in-flight request; normal `Close` still flushes |
| **new** `DialogueSession.Interrupt` | reconcile the latest interrupted assistant turn | Mistral commits only confirmed text and preserves prior user/tool history |
| **new** `DialogueSession.SendControl` | bounded recovery without fake user transcripts | Mistral maps fixed core instructions to a system-role control message |
| `DialogueSession.SendUserTurn` / `SubmitToolResults` | ordinary human turns and tool continuations | Existing protocol and one-in-flight-turn rule remain |
| `Order.Finish` / `ToolExecutor.ValidateOutcome` | final validated result, including repaired outcomes | No storage-port change; exactly-once runner semantics remain |

### 3.2 Domain diff: playback acknowledgement, not VAD provider events

Add one normalized event kind and one opaque field to `domain.CallEvent`:

```go
const (
	EventDialing       CallEventKind = "dialing"
	EventRinging       CallEventKind = "ringing"
	EventAnswered      CallEventKind = "answered"
	EventDTMF          CallEventKind = "dtmf"
	EventPlaybackMark  CallEventKind = "playback_mark"
	EventEnded         CallEventKind = "ended"
	EventError         CallEventKind = "error"
)

type CallEvent struct {
	CallID     CallID        `json:"call_id"`
	Kind       CallEventKind `json:"kind"`
	DTMF       string        `json:"dtmf,omitempty"`
	EndReason  EndReason     `json:"end_reason,omitempty"`
	At         time.Time     `json:"at"`

	// PlaybackID is set only for EventPlaybackMark. It echoes the opaque ID supplied to
	// CallSession.MarkPlayback after all outbound audio queued before that mark has been played.
	PlaybackID string `json:"playback_id,omitempty"`
}
```

The implementation must show the field in the actual struct, not as an embedded or parallel side
channel. IDs are bounded opaque strings (core-generated, at most 64 ASCII characters), never text,
patient data, provider SIDs, or secrets.

**Justification:** playback acknowledgement is telephony progress and belongs in the existing event
normalization path. VAD onset, end-of-turn, barge-in, and hold are **not** new `CallEventKind`s:
they are core observations, not provider call progress. Keeping them as internal agent events avoids
requiring every telephony adapter to emit semantics it does not own.

### 3.3 Telephony port diff: required, provider-neutral playback control

Add these two methods directly to `ports.CallSession`:

```go
// ClearAudio discards outbound audio queued locally or by the provider but not yet played.
// It does not close the media stream or end the call. Audio already played cannot be recalled.
// It is idempotent and ordered against SendAudio/MarkPlayback: audio accepted after a successful
// clear belongs to the new playback generation. Late marks from an older generation may arrive.
ClearAudio(ctx context.Context) error

// MarkPlayback queues an opaque marker after all previously queued outbound audio. The adapter
// emits exactly one EventPlaybackMark with the same PlaybackID when the provider reports that
// point was reached. This is playback evidence, not proof that a person heard the audio.
// IDs are unique within a session. MarkPlayback is ordered with SendAudio and ClearAudio.
MarkPlayback(ctx context.Context, playbackID string) error
```

`SendDTMF` is unchanged. There is deliberately no Twilio-named `Clear`, `StreamSID`, `MarkName`, or
capability payload in the port.

**Why required methods instead of a Twilio-only type assertion:** the only production and stub
providers both implement the capability, and silent optional degradation would make barge-in appear
successful while old audio continues. All call-session fakes and the stub must be updated in the
same commit. A future provider that cannot honor playback clear/mark must fail construction or
return an explicit error; it must not silently acknowledge audio it did not play.

### 3.4 Dialogue and speech port diffs: interruption, control, and abort

Add these methods to `ports.DialogueSession`:

```go
// Interrupt reconciles the most recent assistant turn after its turn context was canceled and
// Wait returned. confirmedText is the exact concatenation of complete assistant segments whose
// playback marks were observed before ClearAudio; it is not the full generated response and never
// contains an estimated partial word. The adapter retains the triggering user/tool history, drops
// unexecuted calls/outcome from the interrupted assistant turn, and records only confirmedText as
// assistant speech. Empty confirmedText means no assistant text was confirmed. Already executed
// tool history is never erased. Interrupt reopens a finish_call-completed session when the core
// interrupted before returning that outcome.
Interrupt(ctx context.Context, confirmedText string) error

// SendControl starts one assistant turn from a fixed core-owned control instruction. It is not a
// transcription and must not be represented as something the far end said. The adapter preserves
// the candidate user/tool messages of a failed or stalled turn when retrying it. The same streaming
// AssistantTurn contract and ordinary tool/outcome limits apply. instruction must be non-empty.
SendControl(ctx context.Context, instruction string) (AssistantTurn, error)
```

Mistral maps interruption metadata and `SendControl` instructions to system-role protocol messages;
only actual STT finals become user-role messages. The adapter retains the last failed candidate
until either a successful retry or `Close`, so a timeout does not silently drop the user turn or tool
results that caused it. `pendingCalls` and `finished` are adjusted so discarded model calls cannot
later execute, while previously committed tool results remain.

**Why two small operations:** canceling a context currently rolls back the entire Mistral candidate,
and using `SendUserTurn` for repair would invent user speech. A generic event bus or public history
editor would be broader than C4 needs. No change is made to `AssistantTurn`, `TurnResult`, tool call
IDs, or the outcome schema.

Add one method to `ports.SpeechStream`:

```go
// Abort stops generation without flushing pending text. It discards queued/unread synthesis,
// closes Audio after bounded transport cleanup, and is idempotent. It is safe after normal Close
// and safe after call cancellation. Abort's context bounds cleanup only; it must not be used to
// continue synthesis. Audio chunks already returned to the core may still exist and must be
// discarded by the caller's playback generation.
Abort(ctx context.Context) error
```

**Why this is necessary:** the current C2 realtime stream uses the `Speak` context for setup, then
creates an independent session context; `Close` intentionally drains queued text through
`speech.audio.done`. Offline mode similarly starts its HTTP request on `Close`. Therefore canceling
the agent's generation context and calling `Close` does **not** cancel in-flight synthesis. Normal
`Close` semantics stay unchanged; `Abort` is the explicit non-flushing barge-in path. Implement it
for realtime, offline, stub, and every fake. Realtime abort cancels the stream session and closes the
WebSocket; offline abort before `Close` emits no request, and abort during synthesis cancels the
request/response body. Serialize `Abort`/`Close`/`FeedText` so no send or audio-channel close panics.

### 3.5 Agent/runner internal contracts and event ownership

The agent's narrow session grows only the capabilities it now legitimately uses; it still cannot
hang up or consume the telephony event channel directly:

```go
type AudioSession interface {
	Inbound() <-chan domain.AudioChunk
	SendAudio(context.Context, domain.AudioChunk) error
	SendDTMF(context.Context, string) error
	ClearAudio(context.Context) error
	MarkPlayback(context.Context, string) error
}

type Engine interface {
	Run(ctx context.Context, order domain.Order, call AudioSession,
		ended <-chan domain.CallEvent, control <-chan domain.CallEvent) (domain.Outcome, error)
}
```

`ended` remains a capacity-one, guaranteed terminal signal. `control` is a capacity-64 relay channel
containing only post-answer `EventDTMF` and `EventPlaybackMark`. The runner remains the sole consumer
of `ports.CallSession.Events` and continues draining it through closure.

Control delivery must be bounded without silent success after loss:

- Relay DTMF/marks in order with a nonblocking send. If the control buffer fills, record a provider
  contract/relay-overflow error and close `control`; the agent must fail the attempt rather than
  navigate with missing digits or reconcile incomplete playback evidence.
- Do not drop individual digits, reorder marks, or put terminal events behind a full control queue.
  `EventEnded` still goes to `ended`; `EventError` still records provider failure.
- Unexpected pre-answer playback marks are contract errors. Post-answer progress duplicates remain
  runner concerns. Late marks from cleared generations are valid wire races and are ignored by ID.
- Agent shutdown closes/cancels its audio worker(s) first, then the relay is shut down by the runner.
  No channel is closed by both producer and consumer.

### 3.6 Conversation state and boundary rules

Use one explicit per-call state machine; do not let independent goroutines each decide whether the
user is speaking:

| State | Inbound policy | Transition/boundary |
|---|---|---|
| `listening` | normal VAD; no outbound assistant audio | 60 ms confirmed speech onset opens one utterance worker; listen timeout remains 20 s |
| `utterance` | pre-roll + live frames feed STT; results drain concurrently | 700 ms hangover after ≥200 ms active speech closes STT end-of-input; 15 s forces a boundary |
| `assistant` | full-duplex VAD from `SendUserTurn`/`SubmitToolResults` start through final playback mark | 60 ms stronger speech onset emits barge-in; cancel model/TTS and clear playback |
| `tools` | keep receiving/transcribing at most one bounded pending utterance; never cancel an already-started atomic tool mutation | finish tool result continuation before a resulting assistant can speak; pending speech can then interrupt it |
| `holding` | detect speech/silence/DTMF, but do not send hold audio to STT or dialogue | speech resumes normal VAD; 500 ms silence returns to listening; 90 s continuous hold ends with `failed/other` |
| `ivr_wait` | after an evidence-backed digit, wait for a new menu, hold, or human speech | repeated/ambiguous/looping menus and the 15 s post-digit timeout follow §3.9 |
| `terminal` | no new STT/TTS/dialogue work | cancel/close workers and let runner persist exactly one outcome |

End-of-turn and barge-in use the same utterance pre-roll, but different onset thresholds. Hold is
not a special kind of silence and never supplies a transcript. A terminal event always wins over a
simultaneous recovery, digit, mark, or new utterance.

### 3.7 Streaming VAD / end-of-turn specification

All DSP uses stdlib `math`, `encoding/binary`, and fixed-size/small bounded slices. No audio package,
CGO, WebRTC/Pion, model, or global mutable DSP state is permitted.

#### Frame features

For every complete 20 ms canonical frame:

1. Decode 320 signed little-endian samples to normalized `float64` (`sample / 32768`), subtract the
   frame mean to remove DC, and compute RMS. Reject empty/odd inbound chunks at the existing audio
   contract boundary; reframe arbitrary chunks without losing residual bytes.
2. Compute zero-crossing rate on DC-removed samples.
3. Apply a Hann window and compute Goertzel power at 100 Hz spacing from 100 through 3900 Hz (39
   bins), plus explicit 350, 440, 480, and 620 Hz tone probes. This is roughly 13k multiply/add
   operations per frame and needs no FFT dependency. Reuse coefficient tables; do not allocate a
   large spectrogram.
4. Compute normalized positive spectral flux against the previous 39-bin magnitude vector:
   `sum(max(0, current-previous)) / (sum(previous)+1e-12)`. Compute spectral flatness as the
   geometric/arithmetic power mean with epsilon `1e-12`. Store only the previous vector and bounded
   windows, not a call recording.
5. A strong hold-tone frame has a probe at least 16× the median spectral power (about 12 dB) and a
   compatible zero-crossing cadence. Such a frame is never speech onset, even though voiced speech
   can also be tonal.

#### Noise adaptation and speech classification

- Normalize an initial noise RMS of `0.003`, clamp adapted noise to `[0.001, 0.05]`.
- On non-speech, non-hold frames only, update upward with `0.02` and downward with `0.005`:
  `noise = (1-alpha)*noise + alpha*rms`. Freeze during speech, hold, and assistant playback so the
  assistant's own echo cannot train the floor.
- Normal energy threshold: `clamp(3*noise, 0.012, 0.10)` (minimum RMS about 394 PCM units).
- A speech-onset frame requires energy above threshold, spectral flatness `<0.75`, flux `>=0.04`,
  and no strong hold-tone match. While already in an utterance, continuation requires energy above
  threshold and no hold-tone match; flux is not required for sustained vowels.
- During `assistant`, barge-in requires **3 consecutive barge frames / 60 ms** above
  `max(normalThreshold, 4*noise, 0.024)`, with flatness/tone checks but **no per-frame spectral
  flux requirement**. Normal listening onset keeps flux. Measured real Mistral TTS speech runs
  are only 3–4 loud frames before consonant/phoneme dips (e.g. RMS 0.0046, 0.0373, 0.0946,
  0.0897, 0.0366, 0.0035, …), so a 5-frame AND-flux rule was effectively unreachable for real
  speech; the stronger energy floor plus tone exclusion preserves barge asymmetry. This is not
  echo cancellation.
- In the normal listening state, start after **3 consecutive onset frames (60 ms)**. Prepend exactly
  **15 frames / 300 ms** of bounded pre-roll, including the onset frames exactly once. Require
  **10 total active frames /
  200 ms** before an utterance can finish.
- End after **35 consecutive non-speech frames / 700 ms**, once minimum active speech exists. Discard
  a noise/cough candidate that never reaches 200 ms and remain in the same listen window.
- Force a boundary at **750 frames / 15 s**. If speech remains active, the next utterance may start
  from continuing speech using the current pre-roll; do not require an artificial silence gap.
- Reset inactivity timers after a real utterance/hold exit. Maximum call duration remains 5 minutes
  and is never extended by VAD, hold, IVR, or recovery activity.

These are initial reviewable constants, not claims of production-grade accuracy. Synthetic tests
establish determinism; the real-phone gate must tune them through a dated plan amendment before any
env surface is considered.

#### Partial and final latency

Open STT at confirmed onset rather than waiting 700 ms after the user stops. Burst only the bounded
300 ms pre-roll to the worker, then feed frames as they arrive; remove C3's whole-utterance replay
from the production path. `ReplayPace` may remain only as a test/fake timing knob if needed, but must
not delay live inbound forwarding.

Each utterance worker opens one STT stream, drains `Results()` from start, retains only the latest
cumulative partial for diagnostics/tests, and never concatenates partials. A partial or early final
never triggers end-of-turn or a dialogue request. VAD hangover calls `Close` as end-of-input; exactly
one final after close drives the conversation. Normal close/final drain uses the revised 35 s agent
cleanup timeout, exceeding both C2 STT's 15 s and realtime TTS's 30 s final-drain limits.

Use a bounded channel from audio pump to worker so network handshake does not cancel playback late or
block inbound consumption silently. At most one active utterance worker and one closing/final worker
may exist; one final user utterance may be pending. Overflow is an explicit conversation/provider
error, never drop-oldest audio presented as a complete transcript. On terminal/cancellation, stop
feeding, close with an independent bounded cleanup context, drain results, and join workers.

### 3.8 Barge-in, playback marks, and history reconciliation

#### Segmenting text for truthful evidence

C3's single TTS stream cannot say which generated words correspond to played audio. C4 therefore
splits streamed assistant text into **bounded speak segments**:

- Accumulate deltas without logging them. Emit at sentence-ending `.?!`/newline boundaries; at 240
  Unicode code points prefer the last comma/semicolon/space, otherwise hard-split at 240.
- Flush a final nonempty remainder when `TextChunks` closes. Empty/tool-only turns open no TTS stream.
- Each segment gets its own `SpeechStream`, text exactly equal to that segment, and a unique marker
  such as `a<turn>-<segment>`. Close TTS while its audio drainer remains active; after the last
  `SendAudio`, call `MarkPlayback`, then await its normalized mark before treating that segment as
  confirmed. At most one segment awaits a mark, so storage is bounded.
- A 30 s per-segment acknowledgement timeout is a provider/playback error. Starting the next segment
  need not wait for model generation of the whole response, but tools from that assistant turn are
  not executed until all its spoken segments complete normally.

Per-segment TTS intentionally trades some prosody/handshake efficiency for an exact mapping between
text and **complete provider-played segments**. Do not estimate spoken prefixes from audio duration,
text deltas, token counts, or rune ratios.

#### Interruption sequence

On confirmed speech onset in `assistant`:

1. Publish barge-in immediately, before any STT network handshake. Preserve the utterance's pre-roll
   and continue inbound audio. Tonal/music frames do not use this path.
2. Cancel the per-assistant-turn context to stop the in-flight Mistral dialogue request, and start
   `SpeechStream.Abort` with an independent **2 s interruption** context. Abort does not flush pending
   text. Keep draining only until `Audio()` closes, discarding every late/old-generation chunk rather
   than sending it. Do not wait for a stuck abort before clearing playback. An abort that cannot
   release the stream within its bound is an operational error.
3. Concurrently with the bounded abort, call `ClearAudio` once with its own 2 s interruption context.
   The Twilio
   adapter increments a local playback generation, drops queued old audio/residual encoder bytes,
   and sends the wire `clear`. New audio is ordered after that clear. If clear fails, abort with an
   operational error instead of speaking over stale playback or claiming barge-in worked. Join the
   abort before any replacement playback; no new assistant audio starts until abort and clear finish.
4. Drain immediately available older marks, then snapshot only complete segments acknowledged before
  clear. Late/duplicate/out-of-order marks are ignored or rejected by generation/sequence; a mark for
   a later segment can never confirm an earlier unmarked one.
5. Wait for the canceled dialogue turn to finish its rollback, then call
   `DialogueSession.Interrupt(confirmedText)`. Preserve triggering user/tool history and discard the
   interrupted turn's unexecuted calls/outcome. Do not re-execute prior booking tools.
6. When the barging utterance's single final transcript arrives, process it as the next real user
   turn. Empty audio/transcript gets the existing bounded repeat behavior; no synthetic user text is
   invented.

A segment currently playing but not yet marked may have been partially heard. Its text is omitted
from confirmed assistant history and the interruption metadata states that the answer was cut off.
This is deliberately conservative: the model may repeat necessary information, but it must not
believe unplayed text was definitely said. Whole-segment marks do not prove human perception.

#### Twilio adapter rules

Keep all wire details inside C1's adapter:

- Outbound messages are `{event:"clear", streamSid:...}` and
  `{event:"mark", streamSid:..., mark:{name:...}}` on the existing bidirectional stream.
- A single per-stream write serializer orders media, mark, and clear. `ClearAudio` increments an epoch
  and purges `outQ`; writer/encoder paths tag and recheck the epoch so an old chunk racing after clear
  cannot be emitted. Hold the playback ordering lock across the bounded clear write, not across TTS
  or network dialogue operations.
- With no attached stream, clear local queued playback and return success; an old disconnected
  Twilio buffer cannot outlive its old stream. After call end, return the session's established safe
  end semantics. Never emit a local mark merely because `MarkPlayback` was called on Twilio.
- Inbound `mark` becomes `EventPlaybackMark` with the exact opaque ID. Unknown IDs are debug-logged
  and ignored by the agent; clear does not guarantee old marks will not arrive.
- Existing inbound DTMF, µ-law conversion, `stop`-is-not-call-end, and DTMF reconnect behavior remain.

Fixed greeting/repeat prompts use the same marks but are not fake dialogue history. Split the greeting
into an AI-disclosure segment and a purpose segment. Track which was confirmed, and create/reconfigure
the dialogue session with a prompt variant that requires any missing disclosure/purpose in the first
real assistant response. Never retain C3's assertion that the entire greeting was spoken after it was
cleared.

### 3.9 DTMF through IVRs: evidence-gated deterministic policy

The LLM does not get a free-form `press_digit` tool in M1. A core parser/policy owns menu evidence and
calls the existing `SendDTMF` with **one digit at a time**.

#### Parser

Work only on final transcripts, never partials. Normalize lowercase, German umlauts, punctuation,
whitespace, and number words (`null`…`neun`, `zero`…`nine`, `Sternchen`, `Raute`). Recognize bounded
patterns in either order, for example:

- `drücken/wählen Sie (die) 1 für Termine`;
- `für einen Termin drücken Sie 1`;
- `press 1 for appointments` / `for reception press 0`.

A digit must be adjacent to an explicit menu imperative and an option purpose; numbers in dates,
phone numbers, office hours, or ordinary speech are not options. Accept only `0-9`, `*`, `#`; do not
infer multi-digit extensions or send patient identifiers.

Target intents are appointment/reception (`termin`, `terminvereinbarung`, `anmeldung`, `rezeption`,
`ordination`, `appointment`, `reception`) with explicit exclusions for emergency-only routes and
obviously unrelated destinations (`rezept`, `rechnung`, `labor`, `befund`, `billing`, `records`).
Keep the keyword list small, testable, and core-owned. A unique matching target option is required;
two different plausible digits are ambiguous and must not be guessed.

#### Navigation policy and evidence

1. A syntactically complete menu transcript enters `ivr` mode without being sent to dialogue as human
   speech. Send a digit only for a unique target option in the **current** menu. Record menu digest,
   digit, step, and time internally; do not log transcript text.
2. Clear the current menu after a successful `SendDTMF`. Never press twice from the same evidence.
   Wait for new final speech, hold, or a human turn. DTMF success means only that the port accepted
   the request, not that the menu advanced.
3. Inbound `EventDTMF` is normalized evidence about the far end, stored in a bounded per-call ring
   (16 events) and never automatically echoed back. It is not proof that our outbound digit worked.
4. Human speech without menu syntax exits IVR mode and goes through normal dialogue. Hold audio uses
   §3.10. An IVR-only dead end need never create a dialogue session.
5. Return `failed/ivr_dead_end` with a fixed non-secret detail when:
   - a confirmed menu has no target or only ambiguous targets after its second presentation;
   - the same menu repeats after an already-sent digit, a menu loop is detected, or four DTMF steps
     would be exceeded;
   - 15 s after a sent digit there is neither new menu/hold/human activity, or only the same menu;
   - the menu explicitly offers no route and repeats.
6. A `SendDTMF` operational/unsupported error is `failed/error`, **not** `ivr_dead_end`; no route was
   navigated. Ordinary non-menu conversation, one unintelligible transcript, silence alone, or hold
   alone is not enough to claim an IVR dead end.

The agent may return the structured `ivr_dead_end` outcome directly; the runner still owns hangup and
persistence, and its later local `EndAborted` must not replace that valid agent outcome.

**Live gate:** after the Twilio upgrade, rerun the C1 outbound-DTMF path against a controlled test IVR,
not a real clinic. HTTP 200, a fake event, a REST call update, or a WebSocket reconnect alone is not
success. Require observed menu advancement attributable to the digit. If the prior HTTP 400 persists,
record the safe Twilio code and leave real-phone M1 blocked; do not add an unproven fallback.

### 3.10 Hold detection and policy (minimal M1)

Hold detection uses the features in §3.7 over a bounded **150-frame / 3 s** sliding window and runs
only when no active user utterance is being transcribed:

- **Silence is never hold.** Digital/near-silence frames update noise normally.
- **Tone hold:** at least 120/150 frames with the same strong 350/440/480/620 Hz tone, or a repeated
  cadence with at least 40 tone frames and two similar on/off bursts. These are initial fixtures, not
  a claim to recognize every carrier tone or to reclassify pre-answer busy signals.
- **Music-like hold:** at least 90% of frames above the adaptive energy threshold, fewer than 20%
  speech-classified frames, nontrivial spectral variation (flux `0.005..0.35`), and at least two
  stable spectral peaks. Steady broadband noise and strong single tones follow their own paths.
- Freeze noise-floor training on hold candidates. Enter hold after one qualifying full window;
  reevaluate each frame rather than launching timers per frame.

On confirmed hold, cancel/clear any fixed or assistant playback, do not open STT for the hold audio,
do not send hold text to dialogue, and do not speak periodic status messages. Continue processing
DTMF and terminal events. Exit on 3 normal speech-onset frames (with pre-roll retained) or 25 frames
/500 ms of silence. A speech exit opens an ordinary utterance stream; a silence exit returns to
listening with a fresh 20 s listen timeout.

**90 s of continuous hold** returns `failed/other` with fixed detail `hold timeout`; it is not
`ivr_dead_end`, not a fabricated `callback_needed`, and not a successful model outcome. Brief exits
reset the continuous timer, but the 5-minute maximum call duration still bounds repeated holds.
This intentionally minimal classifier may miss music or mistake loud line noise for hold; the
real-phone gate and later evidence must tune it.

### 3.11 Structured-outcome repair and recovery bounds

Refactor C3's `drive` into a shared turn runner used by normal, tool-continuation, and recovery turns.
It must always drain text, reconcile playback, await the result, execute ordinary tools at most once,
and apply `ToolExecutor.ValidateOutcome` before returning any outcome.

Recover only these model/conversation failures:

| Trigger | Fixed core control intent |
|---|---|
| `domain.ErrInvalidOutcome` from reserved `finish_call` | correct the invalid outcome; obey the schema and conditional fields |
| `ToolExecutor.ValidateOutcome` rejection | make outcome consistent with actually executed proposal/confirmation state; never invent a booking |
| empty assistant turn (no text, tools, outcome) | produce a useful reply, or finalize with exactly one valid `finish_call` |
| agent-owned 45 s assistant-turn deadline with the call context still alive | resume without repeating prior tool effects, then reply or finalize |

Use `SendControl` with fixed German instructions and a stable internal reason code. Never include raw
provider bodies, model prose, validation-error strings, transcripts, patient values, or credentials
in control text or logs. Transport errors, STT/TTS failures, clear/mark failures, caller cancellation,
and terminal call events are operational/terminal paths, not outcome-repair prompts.

**Maximum: two recovery control rounds per call, total, not two per user turn.** The initial bad turn
does not count; a third consecutive recovery need returns `errOutcomeRecoveryExhausted` wrapped as
`agent.LimitError`, producing `failed/other`. A later valid user turn does not reset this budget.
Ordinary tool rounds still count against `MaxToolRounds=8`; recovery cannot bypass tool validation,
execute a duplicated tool ID, or mutate/clear the existing `bookingExecutor` proposal, confirmation,
or captures.

Recovery turns may stream text and must honor barge-in. If they return ordinary tools, execute them
through the same bounded path; if they return another invalid outcome, consume the next recovery
budget. A successful repaired `booked` still requires the exact matching successful C3 confirmation;
`callback_needed`, `failed`, and `aborted` retain all C3/domain conditional validation. Runner
precedence and exactly one `Finish` do not change.

### 3.12 Escalation/handoff decision

**Keep `escalate_to_human` = `{"ok":false,"error":"unavailable"}` in C4.** A real transfer needs an
operator-owned destination and schedule, consent/disclosure policy, caller/callee terminal semantics,
and a provider-neutral transfer/bridge capability with timeout and failure outcomes. None is designed
or evidenced, and adding a Twilio call-update method here would leak a provider-specific half-feature
into M1. The prompt must continue to say no handoff occurred. This does not conflict with DESIGN's
current tool surface: the tool executes and reports its real capability; real handoff is deferred as
a separately designed production feature.

---

## 4. Layout and dependency law

```text
internal/core/agent/
├── agent.go              per-call state, greeting/disclosure state, limits and main loop
├── audio_features.go     canonical PCM decode, RMS/ZCR, Goertzel powers, flux/flatness
├── vad.go                adaptive noise, onset/hangover, pre-roll and bounded utterances
├── listen.go             continuous pump + bounded utterance workers/final queue
├── transcribe.go         per-worker STT lifecycle; cumulative partial and one final
├── speak.go              bounded text segments, per-segment TTS, mark wait
├── bargein.go            interruption sequencing, playback generation and confirmed text
├── ivr.go                final-transcript menu parser and bounded navigation policy
├── hold.go               bounded tone/music window and hold state/timeout
├── recovery.go           fixed controls, two-round budget and failure classification
├── tools.go              existing four tools; no DTMF/transfer tool added
├── booking_tools.go      existing tool state/consistency preserved
├── prompt.go             IVR/barge/recovery policy + missing-greeting disclosure variants
├── schema.go             unchanged outcome schema
├── errors.go             control overflow, playback, hold, IVR and recovery sentinels
└── *_test.go             core-only fakes and generated canonical PCM fixtures

internal/core/runner/
├── events.go             terminal channel + bounded DTMF/mark control relay
└── *_test.go             relay ordering/overflow/terminal precedence

internal/adapters/telephony/twilio/
├── session.go            playback epoch, clear/mark methods, queue purge and event emission
├── stream.go             outbound clear/mark JSON, serialized writes, inbound mark mapping
└── *_test.go             exact wire messages, races, reconnect/stale generation

internal/adapters/dialogue/mistral/
├── session.go            Interrupt, SendControl, failed-candidate retention/reopen semantics
└── *_test.go             history/commit/rollback/control-role tests

internal/adapters/tts/mistral/
├── realtime.go           non-flushing Abort, WS cancellation and safe audio close
├── offline.go            Abort before/during HTTP SSE synthesis
└── *_test.go             abort/close/feed races and no-request-before-close regression

internal/adapters/stub/
├── telephony.go          real port-compliant local clear/mark behavior
├── dialogue.go           explicit interrupt/control no-op/echo behavior, not fake bookings
├── tts.go                deterministic non-flushing Abort
└── stub_test.go          interface and lifecycle regression

internal/app/
├── app.go                wiring only; no policy
├── m1_integration_test.go real C2 adapters + fake Mistral WS/SSE + scripted fake telephony
├── m1_live_test.go        //go:build live; real-Mistral no-phone gate
├── m1_phone_live_test.go  //go:build live; separately gated real-phone M1 evidence
└── app_test.go            constructor regression

Makefile                     live-test-m1 / live-test-m1-phone; verify stays credential-free
README.md, docs/agent.md, docs/architecture.md
PROGRESS.md                  implementation-session status and observed evidence only
plans/05-conversation.md     read-only except dated Amendments once coding starts
```

**Intentionally unchanged:** STT ports, Mistral STT/TTS wire protocols, normal TTS `Close` flush
semantics, outcome schema, storage/http/payment/notification ports, scheduler/API surface, `go.mod`,
and `go.sum`. The sole speech-port addition is `SpeechStream.Abort`. There is no
new config production struct unless a later amendment adds evidence-backed env settings.

```text
internal/core/domain  → stdlib only
internal/core/ports   → domain + stdlib
internal/core/agent   → core/{ports,domain} + stdlib/log/slog
internal/core/runner  → core/agent + core/{ports,domain} + stdlib/log/slog
internal/app          → core + adapters (composition/integration tests only)
adapters              → core ports/domain; never agent/runner/app or another adapter
```

Core tests keep local fakes and must not import `internal/adapters/stub`; `make check-deps` remains a
required gate. Every goroutine has an owner, bounded queue, cancellation path, and joined cleanup.

---

## 5. Config and environment surface

**Add no `INSTEADLY_*` variables in C4 and make no `Summary()` changes.** Despite real Twilio call
attempts and the real-Mistral no-phone pass, there is still no successful real Media Streams
conversation from which to tune VAD/barge/hold thresholds. Publishing dozens of DSP knobs now would
create an unsupported compatibility surface and invite unsafe per-environment divergence.

Keep defaults as reviewable core constants and override them only through `agent.Config` in unit,
integration, and live tests. Group the superseded M0 gate knobs rather than adding env fields:

```go
type VADConfig struct {
	PreRollFrames       int           // 15
	StartFrames         int           // 3
	BargeInFrames       int           // 3 (60 ms; no per-frame flux)
	MinActiveFrames     int           // 10
	EndSilenceFrames    int           // 35
	MaxUtteranceFrames  int           // 750
	MinSpeechRMS        float64       // 0.012
	BargeInMinRMS       float64       // 0.024
	NoiseRatio          float64       // 3.0
	BargeInNoiseRatio   float64       // 4.0
	MinSpectralFlux     float64       // 0.04
	MaxSpectralFlatness float64       // 0.75
}

type HoldConfig struct {
	WindowFrames       int           // 150
	ExitSilenceFrames  int           // 25
	MaxContinuous      time.Duration // 90s
}

// Agent Config additions/replacements:
VAD                 VADConfig
Hold                HoldConfig
AssistantTurnTimeout time.Duration // 45s; model result, not playback duration
PlaybackAckTimeout   time.Duration // 30s per segment
InterruptionTimeout  time.Duration // 2s for concurrent Abort and ClearAudio
MaxOutcomeRecovery   int           // 2 per call
MaxIVRSteps          int           // 4
IVRWaitTimeout       time.Duration // 15s after an accepted digit
```

Remove the old flat M0 gate fields (`SpeechThreshold`, `PreRollFrames`, `StartFrames`,
`MinActiveFrames`, `EndSilenceFrames`, `MaxUtteranceFrames`) in the same refactor rather than keeping
two competing sources of VAD configuration. Existing `ListenTimeout=20s`, `MaxUserTurns=12`,
`MaxToolRounds=8` and `MaxCallDuration=5m` remain. Raise `CleanupTimeout` from 20 s to **35 s**:
the C2 realtime TTS adapter has a 30 s final-drain limit (STT is 15 s), and C4's more frequent
per-segment closes must not be preempted at 20 s. This is normal resource cleanup, not the separate
2 s barge-in abort/clear bound or the runner's unchanged 5 s hangup/finalization timeout. Zero values
use defaults; negative/impossible combinations are rejected
(for example pre-roll/max frames, minimum active vs. maximum utterance, nonpositive ratios, and
barge-in onset shorter than normal onset is allowed but its threshold must not be weaker). Tests must
cover every default/boundary without serializing these values into health/config summaries.

If real-phone evidence later justifies operator tuning, add a small numbered amendment naming the
exact variable, range, default, and observed reason. Do not add speculative VAD/hold/recovery env vars
in the initial implementation.

---

## 6. Implementation steps (do them in this order)

For sandboxed commands, prefix `GOCACHE=/tmp/insteadly-c4-gocache` (and a writable GOMODCACHE only if
needed). Never edit `.env` or print credentials.

### Step 0 — Reconfirm baseline and protocol facts

1. Run `go test -count=1 ./...`, `go list -m all`, and `git status --short`; require a clean, green
   baseline. Re-read the Twilio clear/mark reference in §12 if it changed.
2. Inventory every `ports.CallSession`, `DialogueSession`, `SpeechStream`, `agent.Engine`, and
   `AudioSession` implementation, including `//go:build live` tests, before changing an interface.
3. Verify:
   ```bash
   go test -tags live -run '^$' ./...
   go test -count=1 ./...
   ```

### Step 1 — Domain/ports, stubs, and runner relay

1. Add only the §3.2–§3.5 contracts. Update all stubs/fakes/compile-time assertions and the C1/C2
   implementations with minimal temporary behavior only as needed to compile; do not claim adapter
   completion until their dedicated steps pass.
2. Implement the separate terminal/control relay, overflow-as-error semantics, and event ordering.
   Keep pre-answer transitions and finalization unchanged.
3. Verify:
   ```bash
   go test -race -count=1 ./internal/core/runner ./internal/adapters/stub
   go test -tags live -run '^$' ./...
   ```

### Step 2 — Pure DSP and streaming VAD

1. Implement framing/features, adaptive noise, onset/hangover, pre-roll, max utterance, and bounded
   state from §3.7. Keep it independent of channels and providers so table tests are deterministic.
2. Test mathematical invariants and generated silence/noise/speech/tone fixtures before introducing
   STT goroutines. Benchmark the per-frame path enough to catch accidental quadratic allocation, but
   add no metrics dependency.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestAudioFeature|TestVAD|TestNoise'
   go test -race -count=1 ./internal/core/agent
   ```

### Step 3 — Streaming utterance workers and STT lifecycle

1. Replace buffered C3 replay with the continuous pump/worker design. Preserve one-stream-per-
   utterance, latest-cumulative-partial handling, close-as-end-of-input, one final, bounded queues,
   and terminal/cancel cleanup.
2. Prove a partial can arrive before hangover and that it does not cause dialogue or end-of-turn.
   Exercise handshake delay, slow final drain, next utterance while previous closes, and overflow.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestListen|TestTranscribe|TestUtterance'
   go test -race -count=1 ./internal/adapters/stt/mistral ./internal/core/agent
   ```

### Step 4 — Twilio clear/mark and scripted playback telephony

1. Implement adapter-local ordered outbound media/mark/clear messages, playback epochs, queue purge,
   encoder reset, stale-mark handling, reconnect/end behavior, and normalized inbound marks.
2. Build a reusable test-only fake call session that scripts inbound canonical audio, DTMF, mark
   acknowledgements, queued playback, clear counts, stream reconnect, and terminal races. Keep it in
   `_test.go`; the production stub only needs simple port-compliant behavior.
3. Verify exact JSON and concurrent `SendAudio`/`ClearAudio`/hangup under race:
   ```bash
   go test -race -count=10 ./internal/adapters/telephony/twilio -run 'TestStream|TestClear|TestMark|TestConcurrent'
   go test -race -count=1 ./internal/adapters/telephony/twilio ./internal/adapters/stub
   ```

### Step 5 — Segment speaker, barge-in, and dialogue history reconciliation

1. Implement bounded text segmentation, per-segment TTS close/drain/mark order, acknowledgement wait,
   and per-turn cancellation. Implement realtime/offline/stub `SpeechStream.Abort`, then the exact
   barge-in sequence and greeting disclosure tracking. Do not execute a turn's tools before its
   playback completes normally.
2. Add Mistral `Interrupt`, including uncommitted, committed, finish-completed, empty-confirmed-text,
   and stale-mark cases. Preserve actual prior tool history and never retain unconfirmed full text as
   assistant speech.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestSpeak|TestBarge|TestInterrupt|TestGreeting'
   go test -race -count=1 ./internal/adapters/tts/mistral ./internal/adapters/dialogue/mistral ./internal/core/agent
   ```

### Step 6 — Bounded structured-outcome recovery

1. Add the shared turn runner, `SendControl`, fixed recovery instructions, 45 s stall detection, and
   the per-call two-round budget. Keep C3 tool execution/validation atomic and unchanged in meaning.
2. Test malformed/domain-invalid outcomes, booked/tool mismatch, empty turns, stalls, first/second
   recovery success, exhaustion, and operational errors. Ensure recovery cannot duplicate capture or
   confirmation side effects.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestRecovery|TestOutcome|TestToolConsistency'
   go test -race -count=1 ./internal/adapters/dialogue/mistral ./internal/core/agent ./internal/core/runner
   ```

### Step 7 — Deterministic IVR navigation

1. Implement the final-transcript parser, unique target selection, menu digest/history, one-digit
   sends, human exit, repeat/loop/step/timeout limits, and direct `ivr_dead_end` outcome.
2. Keep inbound DTMF as evidence only and operational send failures distinct from navigation dead
   ends. Assert that pure IVR runs do not invent dialogue turns or bookings.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestIVR|TestMenu|TestDTMF'
   go test -race -count=1 ./internal/core/agent ./internal/core/runner ./internal/adapters/telephony/twilio
   ```

### Step 8 — Hold detection and policy

1. Implement the 3 s tone/music window, playback cancellation on confirmed hold, speech/silence exit,
   fresh listen timeout, DTMF/terminal handling, and 90 s direct `failed/other` timeout.
2. Test silence, speech, continuous/cadence tones, music-like audio, noise, false-entry boundaries,
   resume with pre-roll, and continuous vs. total call time.
3. Verify:
   ```bash
   go test -race -count=10 ./internal/core/agent -run 'TestHold'
   go test -race -count=1 ./internal/core/agent
   ```

### Step 9 — Credential-free full-chain integration

1. Add `internal/app/m1_integration_test.go` using real core, real C2 STT/TTS/dialogue adapters,
   loopback fake Mistral WS/SSE, local storage, and the scripted fake telephony from Step 4.
2. Assert wire history and provider messages, not just returned outcomes: streamed STT before close,
   clear after barge onset, only confirmed assistant segments in later chat history, exactly one
   digit for a menu, no STT during hold, and exactly two recovery controls at most.
3. Retain a C3-compatible booked/capture happy path and exactly one runner finalization/attempt.
4. Verify:
   ```bash
   go test -race -count=10 ./internal/app -run 'TestM1Integration'
   go test -race -count=1 ./...
   ```

### Step 10 — Gated live harnesses, documentation, and final verification

1. Add real-Mistral no-phone M1 scenarios using fake telephony and real synthesized inbound speech.
   They must be deterministic about observable events but not assert exact model prose. Keep real
   phone and controlled-IVR tests under independent explicit gates and never run them accidentally.
2. Document state ownership, conservative mark semantics, IVR evidence, hold/retry limits, unavailable
   transfer, and the trial/DTMF blockers. Update PROGRESS only with observed results.
3. Run:
   ```bash
   go test -tags live -run '^$' ./...
   go test -race -count=1 ./...
   go vet ./...
   go list -m all
   git diff --exit-code -- go.mod go.sum
   /Library/Developer/CommandLineTools/usr/bin/make verify
   ```
   Run `golangci-lint` v2.15.0 when available; otherwise report the local skip and rely on CI, not
   silence.
4. Run the explicitly gated no-phone live target only with credentials and approval. Record its exact
   result. Do not run or claim real-phone M1 without the upgraded account, controlled destination,
   and explicit user approval.

---

## 7. Test plan

### 7.1 Core and adapter matrix

| File/package | Required cases |
|---|---|
| `agent/audio_features_test.go` | PCM16 sign/endian/DC removal; digital silence; RMS known vectors; ZCR; Hann/Goertzel known tones; flux and flatness known vectors; arbitrary chunk boundaries; bounded allocation/no NaN on zero power |
| `agent/vad_test.go` | 60 ms onset; cough/60 ms noise rejected; 200 ms minimum; 300 ms pre-roll exactly once; 700 ms hangover; 15 s force and continuing-speech restart; quiet speech over rising/falling noise; frozen floor in playback/hold; tones never speech; malformed audio and timeout |
| `agent/listen_test.go` / `transcribe_test.go` | STT opens at onset; pre-roll burst then live order; partial before Close; cumulative partials not concatenated; early final does not endpoint; exactly one final; empty/multiple final; handshake/feed/close errors; one active + one closing worker; pending-final bound; terminal/cancel and no goroutine leak |
| `agent/speak_test.go` | sentence/240-rune/Unicode segmentation; lazy empty/tool-only TTS; normal close while draining; Abort never flushes; mark after final audio; normal confirmed sequence; ack timeout; late/duplicate/out-of-order mark; TTS/feed/send/abort errors; no tool execution before playback completes |
| `agent/bargein_test.go` | speech while model streams and while TTS plays; 60 ms/no-flux barge threshold; tone/cough no barge; cancel before clear; clear once; old audio discarded; new utterance pre-roll retained; confirmed text only; current unmarked segment omitted; empty confirmed text; clear failure; terminal race; no repeated prior tools |
| `agent/ivr_test.go` | German/English digit-before/after-purpose; number words, `*`, `#`; date/phone/hour not a menu; unrelated/emergency/ambiguous choices; unique target; one send per menu; new menu after reconnect; human exit; repeat/loop/four-step/15 s dead ends; send failure = error; inbound DTMF never auto-echoed; pure IVR creates no booking |
| `agent/hold_test.go` | silence ≠ hold; 350/440/480/620 continuous/cadence fixtures; music-like fixture; speech/noise rejection; 3 s entry; cancel greeting/assistant on hold; no STT/dialogue while holding; speech and 500 ms silence exits; 90 s continuous timeout; DTMF/terminal while holding; max call still wins |
| `agent/recovery_test.go` | schema/domain invalid; tool/outcome mismatch; empty turn; model-only 45 s deadline; fixed control content; first and second recovery success; third need exhausted; per-call budget not reset; tool/capture not duplicated; operational/caller/terminal errors not repaired; valid booked still needs C3 confirmation |
| `agent/agent_test.go` | state transitions and greeting disclosure/purpose variants; empty transcript repeat; existing turn/tool/call limits; cleanup order; error mapping; no audio queue growth across many turns |
| `runner/events_test.go` | DTMF/mark forwarded in order; terminal independent of full control queue; control overflow closes/errors rather than silently succeeding; late progress/error handling; drain through close; exactly one Finish and agent `ivr_dead_end` precedence over local hangup |
| `twilio/stream_test.go`, `session_test.go` | exact clear/mark JSON; media→mark order; epoch purge and encoder residual reset; old racing media absent after clear; late marks; unknown marks; no attached stream/reconnect/end; concurrent SendAudio/Clear/Mark/Hangup under `-race`; existing DTMF and µ-law regression |
| `dialogue/mistral/session_test.go` | Interrupt active uncommitted and already committed turn; empty/partial confirmed text; reopen interrupted finish; discard unexecuted pending calls; retain prior tool results; SendControl uses system not user role; failed candidate retained after invalid/stall; wrong pending/double-turn errors; Close/cancel races |
| `tts/mistral/realtime_test.go`, `offline_test.go` | Abort before text/during generation/after Close; no `input_text.end` or offline flush on abort; WS/request cancellation; Audio closes; queued text not sent; FeedText/Close/Abort races and idempotency; normal Close still flushes |
| `adapters/stub/stub_test.go` | new interfaces compile; clear/mark lifecycle and events deterministic; TTS Abort discards without flush; dialogue control/interrupt do not become fake user speech; defaults remain credential-free |

Use generated canonical PCM fixtures and fake servers; do not check in recordings containing patient
or human data. Do not assert model prose, wall-clock exactness, or scheduler timing. Stress all
concurrency suites with `-race -count=10` and include terminal-vs-audio/control races.

### 7.2 `internal/app/m1_integration_test.go` scenarios

1. **Streaming turn:** fake STT emits cumulative partials while audio arrives; assert one stream per
   utterance, live feed order, 700 ms core endpoint, final-only dialogue, and non-silent outbound.
2. **Barge-in:** script two confirmed assistant segments plus an unmarked third, then inbound speech;
   assert model/TTS cancellation, exactly one Twilio-like clear on fake telephony, discarded old
   queue, and the next fake-Mistral request contains only confirmed text plus the actual new final.
3. **IVR:** script `Drücken Sie 1 für Termine`, assert one `SendDTMF("1")`, simulate reconnect and a
   human offer/confirmation, then assert ordinary tools and a valid booked outcome. Separate cases
   cover ambiguity and loop → `failed/ivr_dead_end`, with zero false booking.
4. **Hold/resume:** script 3 s music-like audio, assert no STT/dialogue during hold, then speech with
   pre-roll and a valid outcome. Separate continuous hold timeout returns `failed/other`.
5. **Repair:** fake invalid `finish_call`, then valid repair; separately return a booked outcome
   without matching confirmation and require a control rather than success. Prove exhaustion after
   two controls and exactly one terminal persistence.
6. **Regression:** C3 capture/booking flow, busy/no-answer/rejected remote outcomes, caller cancel,
   runner close, and no-credential/no-secret logs remain green.

The fake Mistral server must inspect system/user/assistant/tool roles and history, and fake telephony
must model marks/clear/DTMF explicitly. A test that merely returns the desired final outcome without
checking these observations is insufficient.

### 7.3 Live gates

**Real Mistral, no phone (required for C4 code complete):** `//go:build live`, explicit
`LIVE_MISTRAL=1` + `MISTRAL_API_KEY`, fake telephony only. Exercise streaming VAD, one barge-in with
clear/history reconciliation, and at least one repaired or valid structured outcome. Use real TTS to
create scripted inbound audio. Do not require a particular sentence from the model and do not log
keys, prompts, transcripts, patient values, or audio.

**Real phone M1 (separate, currently blocked):** independent `LIVE_M1_PHONE=1 LIVE_TWILIO=1
LIVE_MISTRAL=1`, explicit user approval, public tunnel, upgraded Twilio, and a controlled destination.
Require all of the following observed evidence:

- real `<Stream>` attach and two-way audible canonical audio;
- natural end-of-turn without the operator artificially waiting for C3's buffered replay;
- operator interrupts a long answer; queued speech actually stops and the next response does not
  assume the unplayed suffix was heard;
- a controlled IVR advances only after the expected digit, separately proving the C1 outbound-DTMF
  path (no real clinic);
- controlled hold music is not transcribed as a user and speech resumes afterward;
- one valid terminal structured outcome, exactly one attempt, and no double tool effects.

The IVR may use a separate `LIVE_M1_IVR=1` and controlled test number so an ordinary mobile test never
sends digits to a person. Failure to attach a stream, an HTTP 400, or inability to obtain controlled
IVR/hold evidence leaves DESIGN real-phone M1 incomplete. Record observations; do not infer them from
unit/integration tests.

---

## 8. Definition of done

### C4 code complete (no live phone required)

- [ ] Streaming noise-adaptive core VAD and hangover replace C3's buffered gate; pre-roll, partials,
      one-stream-per-utterance, final drain, and all queues are bounded and tested.
- [ ] Barge-in works with scripted telephony during model/TTS/playback; TTS/model cancel, clear,
      stale-audio suppression, and conservative confirmed-text history reconciliation are observed.
- [ ] Twilio sends real `clear`/`mark` protocol messages and normalizes inbound marks; no provider
      types leak into core.
- [ ] DTMF policy presses only unique evidence-backed target digits; ambiguity/loops/timeouts produce
      honest `ivr_dead_end`; operational send failures remain errors.
- [ ] Hold tone/music is distinguished from silence and ordinary speech in deterministic fixtures;
      hold/resume/timeout policies pass.
- [ ] Invalid/missing/stalled outcomes get at most two recovery controls per call; every C3
      booking/tool consistency check remains green.
- [ ] Escalation remains explicitly unavailable and never claims a transfer.
- [ ] Every port implementation, stub, test fake, and live-tagged file compiles; terminal/control
      races and cleanup pass `-race -count=10` stress suites.
- [ ] Credential-free real-C2/fake-Mistral/fake-telephony integration scenarios in §7.2 pass.
- [ ] The explicitly gated real-Mistral no-phone M1 test has an observed pass, or C4 remains only
      🔵 code-written pending that gate (same standard as C3).
- [ ] `go test -race -count=1 ./...`, `go vet ./...`,
      `/Library/Developer/CommandLineTools/usr/bin/make verify`, dependency-law check, build, and
      live-test compile/skip checks pass. CI golangci-lint v2.15.0 is green.
- [ ] `go.mod`/`go.sum` and `Summary()` are unchanged; no new `INSTEADLY_*` names; docs and PROGRESS
      distinguish fake, real-Mistral/no-phone, and real-phone evidence.

### DESIGN real-phone M1 (separate; currently blocked)

Every observation in §7.3's real-phone list exists on an upgraded Twilio account with controlled IVR
and hold destinations. C4 code complete alone does **not** mark DESIGN M1 done, does not prove C1
outbound DTMF, and does not clear the earlier real-phone M0 blocker.

---

## 9. Risks and decision gates

| Risk/gate | Required handling |
|---|---|
| Assistant echo falsely triggers barge-in; no AEC | Stronger 60 ms/4× noise barge onset (energy/tone/flatness, no per-frame flux), tone exclusion, frozen noise floor; document speakerphone limitation; require handset real-phone evidence before tuning or claiming robustness |
| Mark mistaken for human hearing or exact partial text | Confirm whole segments only; omit unmarked partial text; docs/tests say provider-played, not heard |
| Per-segment TTS adds handshake latency or unnatural prosody | Keep segments bounded but punctuation-first; measure in no-phone live test; do not silently revert to duration-based text guessing |
| Old media races after `clear`; old mark arrives late | Adapter epoch + serialized writes + core generation/sequence IDs; explicit race tests |
| STT handshake/final drain starves inbound audio | Publish barge before handshake, bounded live worker queues, at most two workers/one pending final; overflow fails honestly |
| Runner control queue drops a digit/mark | Capacity 64, overflow closes/errors; never silently navigate/reconcile after loss; terminal channel stays independent |
| IVR parser guesses or presses a destructive/unrelated route | Unique target only, exclusions, one digit per current menu, four-step/repeat/timeout bounds, controlled live IVR evidence |
| C1 `<Play digits>` still fails or reconnect loses stream | Preserve existing adapter error; real M1 blocked; revisit only with safe observed Twilio code, no fake navigation success |
| Hold music resembles speech or line noise resembles music | Conservative 3 s classifier, no transcript while confirmed holding, 90 s bound, real-phone tuning amendment; no universal accuracy claim |
| Recovery loops, double-books, or loses candidate history | Two per-call controls, same tool executor/validator, failed-candidate retention, no operational-error disguise, exhaustion = `failed/other` |
| Greeting cleared but prompt says disclosure happened | Segment and mark AI disclosure/purpose separately; prompt variant requires missing disclosure on first real response |
| A future provider cannot support clear/mark/control | Explicit errors/construction failure, never a silent success; required port implementations and fake contract tests |
| Live tests ring a number or leak secrets accidentally | Independent gates, controlled destinations, no CI inclusion, no `.env` reads in no-phone tests, secret-safe logs |

---

## 10. Size estimate

Approximately **4,500–6,000 lines including tests and documentation**:

- 1,700–2,300 production lines across core DSP/state machine, runner relay, Twilio playback, and
  Mistral history/control;
- 2,500–3,300 unit/integration/race/fake-fixture lines;
- 300–400 documentation/live-harness lines.

Expect **3–4 focused implementation sessions** in the order in §6, not one large unverified edit.
The dependency count remains one external module. If implementation wants word-level interruption,
AEC, real transfer, a generic dialogue-event framework, or production tuning env vars, stop and
re-plan rather than expanding C4 silently.

---

## 11. What C5 plugs into here

C5 orchestration should not need to understand conversation internals:

- `runner.Run(ctx, orderID)` remains the single-attempt invocation and exactly-once persistence
  boundary. No public place-call route is added in C4.
- Terminal outcomes retain existing domain kinds. Scheduler policy can treat `failed/ivr_dead_end`
  and `failed/other` (including hold/recovery limits) differently later using attempt history and
  backoff; C4 does not choose retries or callback times.
- C4 stores no recording, raw audio, partial transcript, menu transcript, or model control log. Blob
  policy/storage remains a later explicitly gated feature; current PCM windows and pre-roll are
  ephemeral and bounded.
- Postgres leases, idempotency, concurrent worker limits, and retry scheduling remain C5. C4's queue
  bounds are per call, not global resource accounting.
- Real transfer remains a separate future design requiring destination/consent/terminal semantics
  and a new telephony capability; C5 must not reinterpret `escalate_to_human=unavailable` as success.
- Any evidence-backed production VAD/hold tuning or metrics surface needs its own small amendment;
  do not expose C4's test knobs automatically.
- Future telephony/dialogue/TTS adapters must implement the new clear/mark/interruption/control/abort
  contracts; C7 swapability tests should include them rather than checking only dial/STT/TTS happy
  paths.

---

## 12. Sources

External sources actually fetched while planning (2026-11-17), plus the repository code cited above:

1. [Twilio Media Streams — WebSocket Messages](https://www.twilio.com/docs/voice/media-streams/websocket-messages)
   — outbound `media`, `mark`, and `clear` on a bidirectional stream; inbound `mark`; clear empties
   buffered audio after the clear message; mark is returned after all audio before it is played;
   inbound DTMF message shape. This justifies the C1 wire work and the need to tolerate late marks.
2. [TwiML Voice `<Play>`](https://www.twilio.com/docs/voice/twiml/play) — `digits` accepts
   `0-9`, `A-D`, `w/W`, `#`, `*`; `w` is a 0.5 s pause and digit timing is 100 ms. C4's core policy
   nevertheless emits only single `0-9`, `*`, `#` menu choices; C1 owns any provider timing/redirect.
3. [Mistral realtime transcription documentation](https://docs.mistral.ai/studio/audio/speech-to-text/transcribe-in-realtime)
   and [Mistral function calling](https://docs.mistral.ai/inference/conversations/function-calling)
   were re-checked. No new server-VAD or provider-history API is adopted from them; the exact STT
   cumulative-partial/end-of-input behavior and chat history semantics in this plan come from the
   already-tested C2 implementation, not from an assumed new API.

If these protocols change before implementation, make a dated amendment with the new observed
contract. Do not silently change the core ports to match a provider-specific wire feature.

---

## 13. Amendments

Implementation-session deviations belong here with date, reason, and verification evidence. In
particular, record any DSP default change, real DTMF observation, mark/clear semantics discovered on
a real stream, or live-only recovery behavior. Do not silently rewrite the approved contract or claim
real-phone M1 from no-phone evidence.

### 2026-11-17 — C4 implementation-session notes (deviations, no live run)

The implementation followed §6 Steps 0–10 with the conductor's exception that **no live test
was run**. The following reviewable deviations/decisions are recorded here and in
`PROGRESS.md`:

1. **No replay pacer.** `Config.Sleep`/`ReplayPace` were removed with the C3 gate. The
   streaming path forwards live frames; no whole-utterance replay exists to pace. Tests use
   real bounded channels/fakes, never a production sleeping knob.
2. **IVR timing uses wall clock.** `ivrState.sentAt` still records the injected `Now` for
   evidence, but the 15 s wait uses a separate `waitUntil` from `time.Now()`; using the
   injected fixed clock made `time.Until` immediately negative and produced a false dead end.
   Covered by `TestIvrWaitTimeoutIsDeadEnd` and the IVR integration scenarios.
3. **Hold classification also runs in `assistant` mode.** §3.10 requires confirming hold
   while fixed/assistant playback is active so that playback is canceled; the noise floor is
   frozen in that state. Hold still never runs over an active transcribed utterance.
4. **Twilio playback queue.** The channel-of-chunks queue became an ordered item queue owned
   by one writer, with a playback epoch notification. This is required for ordered
   media/mark/clear and for `ClearAudio` to succeed/purge while a writer is waiting for a
   reconnect (no attached stream). Epoch rechecks and residual-encoder reset are covered by
   `TestClearAudioWithoutStreamSucceedsAndPurges` and the concurrent race test.
5. **Recovery rollback.** Every recovery (invalid outcome, `ValidateOutcome` rejection, empty
   turn, stall) performs a bounded `Interrupt`/confirmed-text rollback before `SendControl`;
   this is how a successful-but-rejected `finish_call` turn and unconfirmed assistant text are
   dropped while executed tool history/submitted results are retained. See
   `TestRecovery...` and `TestSessionSendControlReopensFinishedAndDiscardsOutcome`.
6. **Confirmed text joining.** Whole marked segments are preserved for markers; `Interrupt`
   receives them joined by a single space. No partial word or duration-based prefix is ever
   estimated.
7. **Music-like hold fixture.** The music path is unit-tested with synthetic feature vectors;
   integration fixtures use a deterministic 3 s continuous 440 Hz tone hold. No audio asset,
   recording, or human data was added.
8. **Unchanged surfaces.** No new `INSTEADLY_*` names, `Summary()` keys, `internal/httpapi`
   changes, `docs/api.md` changes, or module dependencies. `CleanupTimeout` is 35 s;
   `InterruptionTimeout` 2 s; runner cleanup stays 5 s.
9. **Live evidence.** Neither `TestLiveM1NoPhone` nor `TestLiveM1Phone` was run in this
   session, and no secret/.env value was read. The no-phone gate remains the C4 exit gate;
   real-phone M1 remains blocked by the Twilio trial/controlled-IVR prerequisites.

### 2026-11-17 — post-review corrections (PR #6)

A reviewer review of PR #6 found five blocking correctness issues and two contract gaps. All
were fixed on `feat/c4-conversation` before merge; no live test was run.

1. **STT end-of-input now drains accepted audio.** The worker's separate `end` signal could win
   a select against frames already accepted into the bounded feed queue, closing STT with
   pre-roll/live audio unread (observed in CI/repeated runs). `closeInput` now closes the feed
   channel itself; a closed buffered channel yields its buffered frames before closure, so the
   normal end path always forwards every accepted frame before `Close`. New tests:
   `TestUtteranceWorkerDrainsQueuedFramesBeforeClose`,
   `TestUtteranceWorkerSlowFeedStillDrainsAcceptedFrames`,
   `TestListenerDelayedHandshakeDrainsEveryAcceptedFrame`; the observed M0 integration case was
   stress-run `-race -count=100`.
2. **IVR purpose evidence is option-local.** The parser now uses hard punctuation clauses and
   nearest-digit partitions inside a clause; an imperative may be shared across coordinated
   options, but a target/exclusion in a different clause cannot justify a digit. Added
   `terminvergabe` to the target list. New tests:
   `TestIvrOfficeHoursMenuDoesNotPressAppointmentDigit`,
   `TestIvrOptionLocalPurposeSelectsOnlyTheMatchingDigit`.
3. **Human/hold activity cancels the post-digit wait.** A non-menu final and confirmed hold
   clear `ivrState.waitUntil`, so a stale 15 s menu timer cannot fire `ivr_dead_end` after the
   caller entered normal dialogue or hold. New test:
   `TestIvrHumanActivityCancelsPostDigitTimeout` (waits past the original deadline).
4. **Control-relay overflow can never persist success.** After shutting the relay down, the
   runner consults `relay.controlError()` and, if set, persists `failed/error` with
   `detailControlLost` even when the agent raced to a valid outcome. New test:
   `TestRunnerControlOverflowNeverPersistsAValidOutcome`.
5. **Playback confirmation requires nonempty audio.** A segment is marked/confirmed only after
   at least one nonempty canonical frame was successfully sent for that generation; an empty
   provider synthesis returns `errNoPlaybackAudio`, confirms no text, and cannot unlock tools
   or an outcome. New tests: `TestPlayerRequiresNonemptyPlaybackAudio`,
   `TestRunSilentSynthesisDoesNotUnlockTools`.
6. **Fresh cleanup budget at exit.** The listener join and dialogue close each receive a fresh
   `CleanupTimeout` budget created at Run exit, not one created at Run entry that is already
   expired after a long call. New test: `TestRunCleanupBudgetIsFreshAtExit`.
7. **Stricter hold cadence.** Cadence now requires the same dominant probe, at least two
   similar on-bursts, and an internal off gap; a single dominant burst or alternating probes no
   longer qualify. New test: `TestHoldDetectorCadenceRequiresSimilarSameProbeBursts`.

Verification after these fixes is recorded in `PROGRESS.md`; `make verify`, `make cover`,
`-race -count=10` on the required packages, live-test compile/SKIP, lint, dependency and drift
checks were re-run. No live test was run and no secret was read.

### 2026-11-17 — live no-phone harness pacing correction (post-merge)

The conductor ran the gated `TestLiveM1NoPhone` on merged `main` and it failed with
`agent: utterance queue overflow` (then a secondary canceled STT open). Instrumentation showed
this was a **test-harness artifact**, not a production VAD/queue defect: the live harness wrote
the whole synthesized caller utterance (~600 canonical frames) in a tight burst; the worker's
bounded 64-frame feed queue (1.28 s) could not drain while the real STT WebSocket handshake ran
(0.6–0.9 s), so the pump correctly reported overflow. Real telephony delivers 20 ms/frame
(50 fps), which the queue is sized for.

Correction (harness only; **no production VAD/queue/`audioFeedBuffer` change**):

- `integrationSession.trySendFrame` is the shared safe enqueue. `sendFrames` remains the
  instant burst used by credential-free integration tests.
- `internal/app/m1_live_test.go` now feeds the live no-phone caller audio with
  `feedPacedWhileRunning`: one canonical frame per `liveFramePace = 20 ms`, stopping early if
  the runner finishes. Both scripted utterances use the paced path.
- Comments in both harness files record that the live path paces at canonical real time and
  that a multi-second burst is a harness artifact, not a VAD/queue defect.

No live test was run in this fix session. Verification before re-running the gate: `make
verify`, `go test -race -count=10 ./internal/app -run 'TestM1Integration'`, live-tag
compile/SKIP, lint, module and drift checks (recorded in `PROGRESS.md`). The gate remains
pending a fresh conductor run.

### 2026-11-17 — live no-phone script correction (test content only)

The conductor re-ran the gate on merged `main` (`92d8cdf`, with the pacing fix) and it failed
for a different, content-only reason: the first scripted utterance asked the model to book
**and** abort, so the real model returned `finish_call(aborted)` with **no assistant text**.
With zero assistant audio there was nothing to barge, and `awaitAudioGrowth` correctly saw the
call end first (`clears=0`, only greeting frames sent). The STT/dialogue/outcome chain itself
worked.

Correction (test content only; no production change):

- `first` is now exactly `"Guten Tag. Ich möchte bitte einen Termin vereinbaren."` — it
  elicits a spoken reply instead of instructing the model to finish.
- The abort instruction remains only in `second`
  (`"Stopp, ich möchte noch etwas sagen. Bitte beenden Sie den Anruf jetzt mit dem Ergebnis abgebrochen."`),
  fed after the assistant starts speaking so the barge → clear → `finish_call(aborted)` flow
  is exercised.
- `awaitAudioGrowth` now requires only `liveReplyGrowthFrames = 5` new canonical frames
  (100 ms) above the greeting baseline, so a short-but-real first reply is not rejected as
  "no audio".
- The comment above the scripts records why the first utterance must not contain the abort
  instruction.

No live test was run in this fix session. Verification before re-running the gate: `make
verify`, `go test -race -count=10 ./internal/app -run 'TestM1Integration'`, live-tag
compile/SKIP, lint, module and drift checks are recorded in `PROGRESS.md`. The gate remains
pending a fresh conductor run.

### 2026-11-17 — live no-phone barge target correction (test content only)

The conductor's next gate run on merged `main` (`b114c4f`) reached the barge step but observed
no clear. Instrumentation showed both STT finals were correct and the model processed the
second utterance, but the assistant's real reply was short (~2.9 s) and quiet: the acoustic
barge onset did not confirm before the reply ended, and STT even dropped the initial "Stopp".
The failure is a live-harness robustness issue; production barge behavior is unchanged and is
still covered by the synthetic `TestM1Integration` scenario.

Correction (test content only; no production change):

- `TestLiveM1NoPhone` now barges during the **fixed greeting**, a deterministic ~8.8 s
  assistant-audio window, instead of a variable-length model reply.
- After `session.sendProgress()`, it waits only for the first audible greeting frame (not the
  two greeting marks) and then feeds one paced caller utterance:
  `"Stopp, Stopp! Bitte beenden Sie den Anruf jetzt mit dem Ergebnis abgebrochen, denn das ist ein automatischer Test."`
  The repeated cue strengthens the acoustic onset without changing production policy.
- The test asserts a terminal `aborted` outcome, `attempts == 1`, `clears >= 1`, non-silent
  640-byte outbound canonical audio, exactly one hangup, and no key leak. The
  `awaitMarks`/`awaitAudioGrowth` gating on a model reply was removed; streaming VAD is still
  exercised by the paced utterance's real STT final, while barge+clear is exercised by the
  greeting barge.
- A comment above the script records why the harness targets the greeting rather than a model
  reply.

No live test was run in this fix session. Verification before re-running the gate: `make
verify`, `go test -race -count=10 ./internal/app -run 'TestM1Integration'`, live-tag
compile/SKIP, lint, module and drift checks are recorded in `PROGRESS.md`.

### 2026-11-17 — barge-onset VAD tuning (production change)

The conductor's gate run on merged `main` (`35d3512`, greeting barge) still reported `clears=0`.
Direct measurement of real Mistral TTS frames showed the barge rule itself was unreachable for
real speech:

- `"Stopp, Stopp! …"` had ~12 leading near-silent frames (RMS ≈ 0.0001–0.0036) before speech.
- `"Guten Tag. …"` RMS was 0.0046, **0.0373, 0.0946, 0.0897, 0.0366**, 0.0035, 0.0014,
  0.0054, **0.1160, 0.1388, 0.1509, 0.1167** — only 3–4 consecutive loud frames before
  consonant/phoneme dips.
- The old barge rule required **5 consecutive** frames that each also satisfied
  `flux >= 0.04`. Flux only spikes at transitions, so the rule essentially never matched real
  speech; synthetic multi-tone fixtures passed only because their flux stayed high every frame.

Production tuning (authorized by the conductor):

1. Barge onset no longer requires per-frame spectral flux: a barge frame needs
   `rms >= max(normalThreshold, 4·noise, 0.024)`, `flatness < 0.75`, and no strong hold tone.
   Normal listening onset still requires `flux >= 0.04`; ordinary turn-taking is unchanged.
2. `BargeInFrames` default is now **3 (60 ms)** instead of 5 (100 ms), because measured real
   speech offers only 3–4 loud consecutive frames. The barge energy floor remains strictly
   stronger than normal onset, so echo/background asymmetry is retained; this is still not
   acoustic echo cancellation.
3. The live no-phone cue now starts with measurable speech energy:
   `"Guten Tag. Bitte beenden Sie den Anruf jetzt mit dem Ergebnis abgebrochen, denn das ist ein automatischer Test."`
   (the previous `"Stopp, Stopp!"` cue began with ~240 ms near-silence).

Added regression tests: `TestVADBargeFiresOnSteadyLoudVoicedFramesWithoutFlux` (stationary loud
frames have near-zero flux, yet 3 barge frames fire) and
`TestVADBargeFiresOnMeasuredSpeechProfile` (the measured RMS envelope with a consonant dip).
Existing synthetic VAD/listener/barge tests were updated for the 60 ms threshold. `docs/agent.md`
and the plan's §3.7/§5/§7/§9 text now state the tuned rule. No live test was run in this fix
session; fake-only verification is recorded in `PROGRESS.md`.

### 2026-11-17 — live no-phone outbound-audio accounting (test harness only)

The conductor's gate run on merged `main` (`d56ec91`, tuned barge) succeeded at the barge:
`clears >= 1` and a terminal `aborted` outcome. The remaining failure was a harness accounting
bug: `"no real outbound audio"`. `integrationSession.ClearAudio` correctly models “clear queued
playback” by setting `sentAudio = nil`; the live test then copied `sentAudio` **after** the
barge clear and found no frames, even though the greeting audio had been sent and played.

Correction (test harness only; no production change):

- `integrationSession` now keeps cumulative outbound evidence that `ClearAudio` never resets:
  `sentAudioTotal`, `sentAudioNonZero`, and `sentAudioBadLen`. `SendAudio` updates them for
  every frame; the existing queued `sentAudio` slice keeps its prior semantics for the other
  fixtures.
- `TestLiveM1NoPhone` asserts `sentAudioTotal >= 1`, no non-canonical frame length, and at
  least one non-zero sample from the cumulative fields (captured under `session.mu` alongside
  clears/hangups). The log line uses the cumulative total.
- Added `TestIntegrationSessionOutboundAccountingSurvivesClear`, a credential-free regression
  that sends one canonical non-silent frame, calls `ClearAudio`, and asserts the queued slice
  is empty while the cumulative total/non-zero evidence survives.

No live test was run in this fix session. Verification before re-running the gate: `make
verify`, `go test -race -count=10 ./internal/app -run 'TestM1Integration'`, live-tag
compile/SKIP, lint, module and drift checks are recorded in `PROGRESS.md`.
