# Chunk C2 — Mistral AI adapters (STT, TTS, Dialogue)

> **Status:** planned, not started. Chunk owner: next coding session.
> **Source of truth:** [DESIGN.md](../DESIGN.md) — this plan derives from it and must not
> contradict it. If a conflict appears, stop and resolve it against DESIGN.md, not silently.
> **Progress register:** [PROGRESS.md](../PROGRESS.md) — update it at the end of the session
> that implements this chunk (see its "Update protocol").
> **Previous chunks:** [C0](01-foundation.md) ✅; [C1](02-twilio-telephony.md) code complete.
> C1's trial-account live-audio blocker does **not** block these credential-free adapter tests.

---

## 1. What this chunk is

C2 replaces the three `mistral` → `ErrNotImplemented` branches in
`internal/app/providers.go` with real implementations behind the **existing**
`ports.SpeechToText`, `ports.TextToSpeech`, and `ports.Dialogue` contracts. Mistral has no Go
SDK, so the adapters speak its documented REST, SSE, and WebSocket protocols directly.

The core remains provider-neutral. Canonical audio remains **PCM16 signed little-endian, mono,
16 kHz**; every output chunk from TTS is exactly `domain.FrameBytes` (**640 bytes = 20 ms**).
Mistral model names, base64 payloads, SSE frames, WebSocket events, and 24 kHz synthesis never
cross a port boundary.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | Realtime STT WebSocket client: canonical PCM16 input, cumulative partial events, final transcript on end-of-input | `internal/adapters/stt/mistral/` |
| 2 | Batch STT multipart client: raw canonical PCM → in-memory WAV → `/v1/audio/transcriptions`; timestamps, diarization, language, hints | `.../stt/mistral/` |
| 3 | Realtime TTS WebSocket client: incremental text in, PCM audio out | `internal/adapters/tts/mistral/` |
| 4 | Offline TTS mode through `/v1/audio/speech` with `stream=true` (SSE), behind the same `ports.SpeechStream` | `.../tts/mistral/` |
| 5 | TTS conversion: Mistral float32 PCM / mono / 24 kHz → canonical PCM16 / mono / 16 kHz, with anti-alias resampling and 640-byte framing | `.../tts/mistral/audio.go` |
| 6 | Dialogue Chat Completions SSE client with local conversation history and streamed assistant text | `internal/adapters/dialogue/mistral/` |
| 7 | Mistral function-calling translation, streamed argument assembly, tool-result continuation, and terminal `finish_call` outcome decoding/validation | `.../dialogue/mistral/` |
| 8 | Exact booking tool/outcome contract for C3 (DESIGN.md §6.1); **contracts only, no tool executor or agent loop** | this plan, `docs/mistral.md` |
| 9 | Strict `INSTEADLY_MISTRAL_*` config, defaults, validation, and secret-safe `Summary` | `internal/core/config/` |
| 10 | Composition-root wiring for all three providers; constructors perform no network I/O | `internal/app/` |
| 11 | Credential-free fake-Mistral HTTP/WebSocket tests, including races, malformed streams, and provider errors | adapter `*_test.go` files |
| 12 | Optional build-tagged, explicitly gated live smoke test (one STT, one TTS, one dialogue tool call); never in CI or `make verify` | `internal/app/mistral_live_test.go` |
| 13 | Operator/protocol documentation and deferred/optional live-verification record | `README.md`, `docs/architecture.md`, `docs/mistral.md`, `PROGRESS.md` |

### Out of scope (do NOT implement — later chunks)

- **Agent engine, conversation loop, VAD, barge-in, DTMF policy, tool execution, or automatic
  multi-tool loop** → C3/C4. `SubmitToolResults` performs exactly one model continuation; the
  core decides whether and how to execute a returned call.
- **A port or domain change.** The existing interfaces are sufficient. In particular, do not add
  `EndTurn`, an error channel, a dialogue event type, or provider-specific options to `ports`.
- **Mistral server-side turn detection as a core mechanism.** DESIGN.md §1.6 keeps VAD and
  turn-taking in the core. C2 leaves `turn_detection` unset; its events are not exposed by the
  current STT port.
- **Realtime diarization, speaker reference clips, voice cloning, emotion tags, pronunciation
  hints, contextual TTS, and the managed Voice Agents SDK.** None is required by the ports.
- **Files API upload/delete, `file_url`, or `file_id` transcription.** C2 sends one multipart
  `file`; no provider-side file lifecycle is needed.
- **Automatic retries/reconnects, scheduler backoff, and rate-limit policy.** Preserve typed
  temporary-error information; retry ownership remains outside these adapters.
- **Recording/transcript persistence, storage changes, payments, notifications, deployment, and
  compliance work.** The adapters must not log or persist payloads on their own.
- **A second AI provider or a shared abstraction over providers.** The existing ports are that
  abstraction.
- **Mandatory live verification as a C2 exit criterion.** Fake providers are the CI proof. The
  live smoke is optional and user-approved even though an unprefixed `MISTRAL_API_KEY` is now
  present; C3/M0 still owns the first real end-to-end telephone conversation.

If any of the above creeps in, stop and note it in the PROGRESS session log instead.

---

## 2. Preconditions (verify first, in this order)

1. **Baseline is green and the worktree is understood.** Planning observed 2026-11-17:
   `go test -count=1 ./...` → every package `ok` (or no test files); `git status --short --branch`
   → `## main...origin/main`, clean. Re-run before implementation:
   ```bash
   cd /Users/q/Insteadly
   git status --short --branch
   go test -count=1 ./...
   ```
   Do not implement C2 on a red baseline. Do not commit unless the user asks.
2. **Re-read the executable contracts.** Read all of `internal/core/ports/{speech,dialogue}.go`,
   `internal/core/domain/{audio,call,tools,order}.go`, the three stubs, and
   `internal/app/providers.go`. Signatures in this plan target those files as they exist on
   2026-11-17, not the earlier prose sketches.
3. **Credential naming, without reading or printing the value.** `.env` now contains the real key
   under the **unprefixed** `MISTRAL_API_KEY`. The app must continue to ignore it; the user owns
   mirroring/exporting it as `INSTEADLY_MISTRAL_API_KEY`. Only the build-tagged live test may read
   `MISTRAL_API_KEY` directly. Check presence, if needed, with `[ -n "$MISTRAL_API_KEY" ]`; never
   echo it.
4. **No new module.** `go.mod` already pins `github.com/coder/websocket v1.8.15`. Its
   `websocket.Dial(ctx, url, *websocket.DialOptions)` supports client connections, custom
   `HTTPHeader`, and an injected `HTTPClient`; it covers both Mistral realtime clients and the
   fake servers. Use stdlib `net/http`, `encoding/json`, `mime/multipart`, and a small local SSE
   reader. Do not add an OpenAI client, Mistral SDK, JSON-schema library, audio library, or SSE
   module.
5. **STT boundary decision (no port change).** `TranscriptionStream.Close` is the caller's
   end-of-input signal and maps to `input_audio.end`; the adapter waits for
   `transcription.done` before returning a successful first `Close`. This is an adapter contract,
   not a hard-coded "one utterance" restriction. C3 will normally open one stream per
   VAD-delimited utterance because that is how it obtains a per-utterance final event.
6. **STT text decision.** Mistral's `transcription.text.delta` contains an append-only delta, not
   a replacement. The port has no delta flag, so emit `TranscriptionEvent.Text` as the
   **cumulative text so far**, `Final=false`; on `transcription.done`, emit its complete `text`
   once with `Final=true`. Never add spaces or punctuation between provider deltas.
7. **TTS wire-format decision.** Request `response_format="pcm"`, not WAV/MP3. Mistral documents
   generated speech as mono 24 kHz and raw `pcm` as headerless; the Mistral integration source
   cited in §12 confirms the raw sample encoding as **float32 little-endian** and converts it to
   int16. Therefore C2's exact wire input is float32 LE mono 24 kHz; it is **not** PCM16 and not
   16 kHz. Convert inside `tts/mistral`. The optional live smoke is the protocol gate for this
   encoding; a discrepancy requires an amendment, never silent guesswork.
8. **No direct Twilio audio reuse.** Twilio's anti-alias downsampler is unexported, works on
   int16 16→8 kHz, and belongs to another adapter. Adapters must not import each other. Implement
   a separate streaming float32 24→16 kHz converter in `tts/mistral`; do not refactor C1.
9. **Dialogue model decision.** Default to **`mistral-small-2603`** (the dated API name for
   Mistral Small 4), configurable via env. It is GA, supports Chat Completions, streaming function
   calling, and structured outputs, and has a suitable speed/cost balance for short German phone
   turns. Pinning avoids an unreviewed behavior change through `mistral-small-latest`; operators
   can opt into another compatible model explicitly.
10. **Outcome decision.** Add a reserved function **`finish_call`** whose parameters are
    `SessionRequest.OutcomeSchema`. The adapter intercepts that function, strictly decodes and
    validates `domain.Outcome`, and returns it in `TurnResult.Outcome` rather than as a tool for
    C3 to execute. This uses Mistral's function-calling JSON schema without mixing JSON outcome
    text into audio-bound assistant content. Full tool execution and order finishing remain C3.
11. **TTS mode decision.** `realtime` is the default and meets "speak text as it is generated".
    `offline` is a supported fallback behind the same port: it buffers text and sends one
    `POST /v1/audio/speech` with `stream=true` on `Close`, so audio cannot arrive before `Close`
    in that mode. This degraded latency must be documented; do not silently fall back after a
    realtime failure (a retry could speak text twice).
12. **Design check.** No conflict with DESIGN.md is required by these decisions. The optional
    live smoke does not change M0's requirement to prove the real chain in C3. If implementation
    discovers that a port cannot honor its documented semantics, stop and flag the proposed port
    change; do not edit it opportunistically.

---

## 3. Mistral → port mapping

### 3.1 Common endpoint, authentication, and URL rules

Production base URL: **`https://api.mistral.ai`**. Adapter `Config.BaseURL` is injectable for
`httptest`; config validation permits only an HTTPS origin in the real app. Trim trailing `/`.
For WebSocket URLs, replace scheme `https` → `wss` (and `http` → `ws` only in direct adapter
tests), preserve host/port, append the exact path, and set only the documented `model` query.

| Capability | Method / protocol | Exact endpoint |
|---|---|---|
| Realtime STT | WebSocket JSON text messages | `/v1/audio/transcriptions/realtime?model=voxtral-mini-transcribe-realtime-3` |
| Batch STT | HTTP multipart POST → JSON | `/v1/audio/transcriptions` |
| Realtime TTS | WebSocket JSON text messages | `/v1/audio/speech/realtime?model=voxtral-mini-tts-3-0` |
| Offline TTS | HTTP JSON POST → SSE | `/v1/audio/speech` |
| Dialogue | HTTP JSON POST → SSE | `/v1/chat/completions` |

Every HTTP request and WebSocket handshake sends:

```http
Authorization: Bearer <API key>
```

REST JSON also sends `Content-Type: application/json`; SSE requests send
`Accept: text/event-stream`. Batch STT uses `multipart/form-data; boundary=...`. Never put the
key in a URL/query, log the header, or include it in errors. Use a shared `http.Client` supplied
in adapter config, or a default with sane transport timeouts; do **not** set a whole-client
`Timeout` that kills long-lived SSE streams.

Opening a WebSocket uses a 10 s handshake timeout, waits for `session.created`, sends the exact
configuration below, and waits for `session.updated`. Thus invalid model/options fail from
`TranscribeStream`/`Speak`, before audio or text is accepted. A non-101 handshake becomes an
`APIError` using the bounded handshake response body. After open, the caller owns the session and
must call `Close`; the opening context bounds setup, not the entire returned session.

All WebSocket JSON messages are text frames. Use `SetReadLimit`: **1 MiB** for STT, **4 MiB**
for TTS audio events. Each write has a 5 s deadline. Ignore unknown event *types* with a debug
log containing only the type; malformed known events are fatal to that session. No automatic
reconnect in C2.

### 3.2 Realtime STT → `ports.TranscriptionStream`

#### Open and configure

`TranscribeStream(ctx, opts)` dials the endpoint in §3.1, receives:

```json
{
  "type": "session.created",
  "session": {"model": "voxtral-mini-transcribe-realtime-3"}
}
```

then sends exactly one update (omit `language` when empty and `context_bias` when empty):

```json
{
  "type": "session.update",
  "session": {
    "audio_format": {"encoding": "pcm_s16le", "sample_rate": 16000},
    "target_streaming_delay_ms": 480,
    "language": "de",
    "context_bias": ["Ordination Dr. Muster", "Hietzing"]
  }
}
```

Wait for `{"type":"session.updated","session":{...}}`; verify the applied audio encoding/rate
when those fields are present. Do not send `diarize`, `speaker_references`, or `turn_detection`.
Configuration is immutable after the first audio append.

`StreamOptions.Language` is BCP-47 at the port, while Mistral requires a two-letter ISO 639-1
code. Normalize with `strings.ToLower`, take the primary subtag before `-`, and require exactly
two ASCII letters. Thus `de-AT` → `de`, `en` → `en`; empty omits the field; malformed or a
non-two-letter primary subtag returns an error before dialing. Hints map verbatim, in order, to
`context_bias`; reject empty hints locally rather than sending empty entries.

#### Feed audio

For every accepted `domain.AudioChunk`, one writer goroutine sends:

```json
{"type":"input_audio.append","audio":"<standard base64 of the exact chunk bytes>"}
```

Canonical input is already `pcm_s16le` mono 16 kHz: **no resampling or codec conversion**. Feed
must copy the caller's byte slice before queuing. Accept any non-empty even-length canonical
chunk (640 bytes is normal); reject empty or odd-length data. A bounded queue of **100 chunks**
(about 2 s at normal framing) provides backpressure. When full, drop the oldest queued chunk,
increment `stt_dropped_chunks`, and log an aggregate warning at close; `Feed` returns when the
new chunk is queued or its context is cancelled. A dropped-audio warning is required because
transcript completeness is no longer guaranteed. Only the writer goroutine sends audio. Sequential
`Feed` calls preserve byte/chunk order; concurrent callers are race-safe, and their wire order is
the queue-acceptance order (no cross-goroutine invocation-order guarantee).

#### Receive text

Typical server events are:

```json
{"type":"transcription.text.delta","text":"Guten"}
{"type":"transcription.text.delta","text":" Tag."}
```

and after end-of-input:

```json
{
  "type": "transcription.done",
  "text": "Guten Tag.",
  "segments": [],
  "language": "de",
  "model": "voxtral-mini-transcribe-realtime-3",
  "usage": {"prompt_audio_seconds": 1.2}
}
```

Mapping:

| Mistral event | Port behavior |
|---|---|
| `transcription.text.delta` | Append `text` verbatim to an accumulator; if non-empty, emit `{Text: accumulator, Final: false, At: time.Now()}` |
| `transcription.done` | Emit exactly one `{Text: event.text, Final: true, At: time.Now()}` (including empty text for silence), then close `Results()` after socket close |
| `session.created` / `session.updated` | Handshake only; never transcript events |
| `turn.*` | Not expected because turn detection is off; ignore as unknown provider events if seen |
| `error` | Store a safe `ProtocolError`, close the socket/results, and return it from `Close` and subsequent `Feed` |
| clean close before `done` while closing | Error: final transcript missing; do not invent a final from partials |
| clean normal close after `done` | Success |

`Results()` is buffered (64 events). If a slow consumer fills it, drop the oldest **partial**
events, never the final; this bounds memory without blocking the WebSocket reader forever. The
reader is the only goroutine that closes `Results()`.

#### Close and errors

The first `Close(ctx)`:

1. stops accepting audio and lets already queued appends drain;
2. writes `{"type":"input_audio.end"}`;
3. reads remaining deltas and waits for `transcription.done` and normal socket close;
4. emits the final event, closes `Results()`, and releases resources.

Use the caller's context plus a maximum **15 s final drain** after the last queued audio has been
written. On timeout/cancellation, close the socket with `CloseNow`, close results through the
reader's deferred cleanup, and return the context/deadline error. Concurrent/repeated `Close`
calls share one close operation and its stored result; `Feed` after close returns
`ErrStreamClosed`. `Close` after an earlier provider failure is still idempotent and returns that
stored failure. There is no error field on `TranscriptionEvent`, so asynchronous failure is
observable through channel closure plus `Close`/subsequent `Feed`; C3 must always call `Close`.

### 3.3 Batch STT → `SpeechToText.TranscribeFile`

`TranscribeFile(ctx, r, opts)` receives **headerless canonical PCM16 LE mono 16 kHz**, not a WAV
file. Read it with a hard cap of **64 MiB + 1 byte** (64 MiB is about 34 minutes at 32,000 B/s);
reject empty input, odd byte count, read failure, or overflow. Build a 44-byte RIFF/WAVE header
in memory with `encoding/binary`:

- `RIFF`, little-endian chunk size `36 + pcmBytes`, `WAVE`;
- `fmt ` chunk size 16, audio format 1 (integer PCM), channels 1;
- sample rate 16,000, byte rate 32,000, block align 2, bits/sample 16;
- `data`, length `pcmBytes`, then the unmodified PCM payload.

Send `POST {BaseURL}/v1/audio/transcriptions` with multipart file field `file`, filename
`recording.wav`, content type `audio/wav`, and these form fields:

| Field | Rule |
|---|---|
| `model` | configured batch model, default `voxtral-mini-transcribe-3` |
| `language` | normalized as §3.2; omitted when empty |
| `context_bias` | repeat once per non-empty hint, preserving order |
| `timestamp_granularities` | exactly `segment` when `opts.Timestamps || opts.Diarization`; otherwise omitted |
| `diarize` | `true` only when requested; this intentionally implies segment timestamps |
| `stream` | omitted/false — the port returns one complete `domain.Transcript` |

Do not use word timestamps, Files API, URL input, realtime turn detection, or SSE for batch.
Limit a JSON response to **16 MiB**. A representative response is:

```json
{
  "model": "voxtral-mini-transcribe-3",
  "text": "Guten Tag. Ich brauche einen Termin.",
  "language": "de",
  "segments": [
    {
      "type": "transcription_segment",
      "text": "Guten Tag.",
      "start": 0.0,
      "end": 1.25,
      "speaker_id": "speaker_1"
    },
    {"type": "transcription_segment", "text": " Ich brauche einen Termin.",
     "start": 1.4, "end": 2.8, "speaker_id": "speaker_2"}
  ],
  "usage": {"prompt_audio_seconds": 2.8}
}
```

Map `text` verbatim. For each segment, convert floating-point seconds to `time.Duration` with
`time.Duration(math.Round(seconds * float64(time.Second)))`; map nullable/missing `speaker_id` to
empty string. Reject negative, non-finite, or `end < start` values. Return an empty **non-nil**
`Segments` slice when timestamps were not requested.
Unknown response fields are ignored for forward compatibility. The core, not this adapter,
decides whether and where to persist the transcript.

### 3.4 Realtime TTS → `ports.SpeechStream`

#### Open and feed text

`Speak(ctx, opts)` with mode `realtime` dials
`/v1/audio/speech/realtime?model=voxtral-mini-tts-3-0`, waits for `session.created`, and sends:

```json
{
  "type": "session.update",
  "session": {
    "voice": "voice_447",
    "streaming_mode": "responsive",
    "response_format": "pcm"
  }
}
```

`voice` is `opts.Voice` when non-empty, otherwise configured `DefaultVoice` (default
`voice_447`). Wait for `session.updated` and verify `response_format == "pcm"` when present.

Each non-empty `FeedText(ctx, text)` enqueues one message, verbatim (including leading/trailing
spaces):

```json
{"type":"input_text.append","text":"Guten Tag, "}
```

Do not insert separators and do not automatically send `flush` on punctuation or pauses. Reject
invalid UTF-8. A single text delta and the total buffered text per session are each capped at
**64 KiB**. One writer goroutine preserves order; its bounded queue holds 64 deltas and blocks
only until space, context cancellation, or terminal failure. Copy the string into the queued
message (Go strings are immutable) and never log its contents.

Server messages:

```json
{"type":"speech.audio.delta","audio_data":"<base64 float32le mono 24kHz bytes>"}
{"type":"speech.audio.done","usage":{"completion_tokens":12}}
```

Decode each `audio_data` independently with standard base64 and pass bytes **in arrival order**
to the streaming converter below. `speech.audio.done` marks completion of the one generation;
it is not an instruction to close the port channel before the final canonical frame is flushed.

#### Float32/24 kHz → PCM16/16 kHz conversion

This is mandatory; `response_format=pcm` minimizes container parsing but does **not** eliminate
conversion.

1. Require each decoded delta length to be a multiple of 4. Decode each sample with
   `binary.LittleEndian.Uint32` + `math.Float32frombits`; reject NaN/Inf. The samples are mono,
   24,000 Hz, normalized approximately to `[-1,1]`.
2. Apply a streaming **15-tap Hamming-windowed sinc low-pass filter**, cutoff **7.2 kHz** at the
   24 kHz input rate (below the 8 kHz output Nyquist), then decimate 3:2. For tap index
   `n=0..14`, center `D=7`, and `f=7200/24000=0.3`, use
   `h[n] = 2*f*sinc(2*f*(n-D)) * (0.54 - 0.46*cos(2*pi*n/14))`, with `sinc(x)=sin(pi*x)/(pi*x)`
   and `sinc(0)=1`; normalize all 15 coefficients to sum to 1. Filter at 24 kHz, then emit output
   phases at input positions `0, 1.5, 3, 4.5, ...`; linearly interpolate adjacent filtered values
   for half-sample phases rather than simply dropping every third sample. Keep history and phase
   across arbitrary WebSocket delta boundaries.
3. Center the filter on the output position (zero history before sample 0, up to seven future input
   samples for the first output). At `speech.audio.done`, supply exactly seven zero samples to
   evaluate positions still inside the original input timeline; emit only positions `< N`, where
   `N` is the original input sample count. Thus no tail is lost and 480 input samples produce
   exactly 320 output samples (not extra tail-only samples). Clamp each resampled value `x` and
   convert exactly: `x <= -1` → -32768, `x >= 1` → 32767, otherwise
   `int16(math.Round(float64(x)*32767))`; encode signed int16 little-endian. Do not convert float
   bytes directly to int16 bytes without resampling.
4. Accumulate canonical bytes and emit only whole **640-byte** chunks. After the final flush,
   zero-pad one residual frame to 640 bytes; if there is no residual, emit no extra silence.
   Every `Audio()` value therefore has `len(Data) == domain.FrameBytes`, including the last.
5. `Audio()` has capacity 100 frames. A slow core applies bounded backpressure to the socket
   reader rather than dropping synthesized speech (dropping would create missing words). The
   caller must run its `Audio()` drainer concurrently with `Close` whenever more than 100 frames
   may remain; otherwise Close would wait for a full channel that nobody drains. The reader stops
   on session cancellation; a non-draining consumer is a port-contract bug and must not leak
   goroutines after `Close`/context cancellation.

The converter is local to this package. Do not import or generalize the Twilio 16→8 kHz int16
filter in C2; sample representation, ratio, and package ownership differ.

#### Close

One `SpeechStream` is one generation. The first `Close(ctx)` stops text input, drains queued
appends, sends:

```json
{"type":"input_text.end"}
```

then drains audio through `speech.audio.done` and the server's normal close (1000), flushes and
frame-pads audio, and closes `Audio()`. Use a **30 s** maximum final drain bounded by `ctx`.
Repeated/concurrent close shares the result. `FeedText` after close returns `ErrStreamClosed`.
Do not use `input_text.flush` in C2: the port has no next-generation operation. An `error` event
or abnormal close fails the stream and is returned by `Close` and subsequent `FeedText`; partial
canonical audio already delivered is not rolled back.

### 3.5 Offline TTS → the same `ports.SpeechStream`

Mode `offline` exists for operators that prefer REST/SSE or cannot use realtime WebSockets. It is
not an automatic fallback and has different latency: `Speak` only constructs local state;
`FeedText` appends to a bounded strings.Builder (same UTF-8/64 KiB total limit); no network or
audio occurs until the first `Close`.

`Close(ctx)` sends exactly one request:

```http
POST {BaseURL}/v1/audio/speech
Authorization: Bearer <key>
Content-Type: application/json
Accept: text/event-stream
```

```json
{
  "model": "voxtral-mini-tts-3-0",
  "input": "Guten Tag, wie kann ich helfen?",
  "voice_id": "voice_447",
  "response_format": "pcm",
  "stream": true
}
```

The selected voice uses the same override/default rule as realtime. Empty/whitespace-only total
input is a local error. The response is SSE:

```text
event: speech.audio.delta
data: {"type":"speech.audio.delta","audio_data":"<base64>"}

event: speech.audio.done
data: {"type":"speech.audio.done","usage":{}}

```

Feed each delta to the **same** float32 → resample → canonical-frame pipeline, in order; done
flushes/pads and closes `Audio()`. Non-2xx responses use the common `APIError`. SSE comments,
CRLF, multi-line `data`, and `[DONE]` follow §3.6's parser rules, although TTS normally uses the
typed events above. No non-streaming `stream=false` client is needed by the port.

### 3.6 Dialogue → `ports.Dialogue` / `DialogueSession` / `AssistantTurn`

#### Session state and request

`NewSession` performs validation only, **no HTTP call**. Require a non-empty `OrderID`, at least
one non-empty system prompt after applying the built-in default, unique valid tool names matching
`^[a-z][a-z0-9_]{0,63}$`, and each `domain.Tool.Parameters` / `OutcomeSchema` to be one valid JSON
object with `"type":"object"`. `OutcomeSchema` is required for this booking adapter. Copy caller
slices/bytes so later mutation cannot alter history.

The built-in prompt is a short German-first medical-booking instruction: identify Insteadly as
an AI assistant, state the call purpose, be concise, never invent patient details or a confirmed
slot, use supplied tools for state changes, and call `finish_call` exactly once at a terminal
result. A non-empty `SystemPrompt` replaces the prose booking instructions, but the adapter
appends a fixed protocol sentence requiring terminal outcomes through `finish_call`; C3 still
owns order/patient context and detailed policy.

Each assistant turn calls `POST {BaseURL}/v1/chat/completions` with:

```json
{
  "model": "mistral-small-2603",
  "messages": [
    {"role": "system", "content": "<prompt>"},
    {"role": "user", "content": "Guten Tag, ich brauche einen Termin."}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "capture_info",
        "description": "Capture one item of patient information.",
        "parameters": {"type": "object", "properties": {}}
      }
    },
    {
      "type": "function",
      "function": {
        "name": "finish_call",
        "description": "Finalize this call with the structured outcome.",
        "parameters": {"type": "object", "properties": {"kind": {"type": "string"}}}
      }
    }
  ],
  "tool_choice": "auto",
  "parallel_tool_calls": false,
  "stream": true,
  "temperature": 0.1,
  "max_tokens": 512
}
```

The actual `parameters` values are the caller's raw JSON schemas, not the abbreviated examples
above. `finish_call` is reserved: reject a caller-supplied tool with that name, then append the
reserved definition once. Disable parallel calls in v0 so tool results and outcome ordering are
deterministic. Temperature and max tokens are adapter constants, not new env surface.

`SendUserTurn` rejects empty/whitespace-only input and appends a `user` message to a candidate
history. `SubmitToolResults` is valid only immediately after a completed turn that returned
ordinary tool calls: require exactly the pending call ID set (no missing, unknown, or duplicate
IDs), require every `Result` to be valid JSON, and append one message per result in pending-call
order:

```json
{"role":"tool","name":"capture_info","content":"{\"ok\":true}","tool_call_id":"call_123"}
```

The previous assistant message (including `tool_calls`) is already in candidate history. Commit
history only after a turn completes successfully; a failed/cancelled HTTP stream leaves the last
committed state unchanged. A session permits one in-flight turn at a time and returns
`ErrTurnInProgress` otherwise. After a valid `finish_call`, the session is terminal and further
turns return `ErrSessionFinished`. `Close` cancels an active request and is idempotent; it makes
no provider request.

#### SSE parsing and streamed text

Require HTTP 200 and a compatible `text/event-stream` content type. Implement a bounded SSE reader
with stdlib only:

- accept LF and CRLF; lines beginning `:` are comments/keepalive;
- concatenate repeated `data:` lines with `\n`; dispatch on an empty line;
- use `event:` only as a cross-check when present; the JSON `type`/chat payload is authoritative;
- ignore unknown fields; malformed JSON in a data event fails the turn;
- `[DONE]` succeeds only after a terminal `finish_reason` was observed; EOF before both is an
  error;
- maximum SSE event/data size **1 MiB**, accumulated assistant text **64 KiB**, each tool's
  arguments **64 KiB**, at most **8 calls** per turn.

Representative chunks:

```json
{"id":"chat_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Guten"},"finish_reason":null}],"usage":null}
{"id":"chat_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" Tag."},"finish_reason":null}],"usage":null}
{"id":"chat_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":20}}
```

For each non-empty string `choices[0].delta.content`, send the exact delta on `TextChunks()` and
append it to the assistant message; do not add spaces. Also accept the documented content-part
form `[{"type":"text","text":"..."}]`, concatenating text parts in order; ignore null content.
Reject multiple choices (the request does not set `n`) rather than merging replies.

Tool calls arrive fragmented. Assemble by **`delta.tool_calls[].index`**, because later fragments
may omit ID/name:

```json
{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"capture_info","arguments":"{\"field\":"}}]},"finish_reason":null}]}
{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"reason\",\"value\":\"Kontrolle\"}"}}]},"finish_reason":null}]}
{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
```

At completion, require non-empty unique IDs and names, `type` absent or `function`, contiguous
indices from zero, and arguments that decode as exactly one JSON object. Preserve compact raw
arguments in `ports.ToolCall.Arguments`. Reject a name not supplied in `SessionRequest.Tools`,
except `finish_call`. The adapter validates JSON shape but does not execute arguments or perform
full JSON Schema validation; C3 must treat every call as untrusted model output.

`finish_reason=stop` and `tool_calls` are valid. `length`, `model_length`, or an incomplete tool
call returns `ErrTurnTruncated` rather than silently accepting missing text/arguments. Close
`TextChunks()` before making `Wait` ready. `Wait(ctx)` returns the stored result/error, or
`ctx.Err()` while the turn is incomplete; consumers still must drain text first. Empty successful
results use empty non-nil `ToolCalls` slices for deterministic tests.

#### `finish_call` outcome extraction

If the sole completed function call is `finish_call`, strictly decode its arguments with
`DisallowUnknownFields` into `domain.Outcome`, reject trailing JSON, apply §3.8's semantic rules,
and return `TurnResult{ToolCalls: []ToolCall{}, Outcome: &out}`. Do not expose `finish_call` as a
`ToolCall` and do not expect a `ToolResult` for it. Combining `finish_call` with any other call,
returning it more than once, or returning malformed/invalid outcome data fails the turn. Ordinary
turns may have tool calls **or** streamed text; any text emitted before a tool call remains part
of history and was already visible to the caller.

### 3.7 Booking tool surface resolved for C3 (DESIGN.md §6.1)

C2's dialogue adapter is generic over `req.Tools`; the following is the provider-neutral contract
C3 must instantiate as `domain.Tool` values. C2 does **not** implement these handlers. Every
schema has `"type":"object"` and `"additionalProperties":false`; timestamps are RFC 3339 strings
with JSON Schema `format:"date-time"`.

| Name | Exact argument properties | Required | Meaning / handler result contract |
|---|---|---|---|
| `propose_slot` | `start` (date-time), `end` (date-time), `doctor` (string), `practice` (string) | `start`, `end` | Propose, never book. C3 checks `start < end` and preference policy. Result: `{"ok":true,"start":"...","end":"..."}` or `{"ok":false,"error":"outside_preferences"}` |
| `confirm_booking` | same four plus `confirmation_code` (string) | `start`, `end` | Record a slot only after explicit clinic acceptance. Result: `{"ok":true,"confirmation_code":"..."}` or `{"ok":false,"error":"not_confirmed"}`. It does not replace terminal `finish_call` |
| `capture_info` | `field` (enum `full_name`, `phone`, `birth_date`, `insurance_number`, `reason`, `note`), `value` (string, 1..2048 chars) | `field`, `value` | Capture one user-authorized field; no arbitrary keys. Result: `{"ok":true,"field":"reason"}` or `{"ok":false,"error":"invalid_value"}` |
| `escalate_to_human` | `reason` (non-empty string), `urgent` (boolean) | `reason` | Ask C3 to hand off; C2 never performs a transfer. Result: `{"ok":true,"action":"escalate_to_human"}` or `{"ok":false,"error":"unavailable"}` |
| `finish_call` | the canonical outcome schema in §3.8 | per schema | Reserved terminal outcome, intercepted by the dialogue adapter; no C3 tool result |

Descriptions must state the side-effect rules above, especially that `propose_slot` is not a
booking and `confirm_booking` requires explicit acceptance. `capture_insurance` is deliberately
not separate: `capture_info(field="insurance_number")` covers it without expanding the tool
surface. DTMF, hold, and barge-in tools are C4 decisions, not hidden C2 additions.

### 3.8 Canonical outcome JSON schema and semantics

C3 passes this exact schema as `SessionRequest.OutcomeSchema`; C2 tests use the same bytes. Keep
it flat (rather than `oneOf`) for broad function-schema compatibility, and enforce conditional
requirements in Go. The schema itself remains core-owned in C3, not imported from an adapter.

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "kind": {
      "type": "string",
      "enum": ["booked", "callback_needed", "failed", "aborted"]
    },
    "detail": {"type": "string", "minLength": 1},
    "booked_slot": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "date_time": {"type": "string", "format": "date-time"},
        "doctor": {"type": "string"},
        "practice": {"type": "string"}
      },
      "required": ["date_time"]
    },
    "callback_at": {"type": "string", "format": "date-time"},
    "fail_reason": {"type": "string", "minLength": 1}
  },
  "required": ["kind", "detail"]
}
```

After strict JSON decode, all kinds require non-blank `detail` and these additional rules:

| `kind` | Required | Forbidden/empty |
|---|---|---|
| `booked` | non-nil `booked_slot`, non-zero valid `date_time` | `callback_at`, `fail_reason` |
| `callback_needed` | non-nil, non-zero valid `callback_at` | `booked_slot`, `fail_reason` |
| `failed` | non-blank `fail_reason` (use normalized `busy`, `no_answer`, `rejected`, `ivr_dead_end`, `error`, or `other` in the C3 prompt) | `booked_slot`, `callback_at` |
| `aborted` | none beyond kind/detail | `booked_slot`, `callback_at`, `fail_reason` |

Use a throwaway `domain.Order{Status: domain.OrderInCall}` and its `Finish(out)` to reuse the
existing domain validation (`booked_slot`, `callback_at`, `fail_reason`, known kind) without
exporting `validateOutcome`; add the blank-detail, zero-time, and irrelevant-field checks in the
adapter. Never mutate or persist the real order here. This resolves §6.1 without changing domain
or ports.

### 3.9 Error, retry, and logging behavior

Each package defines its own equivalent safe error types (do not create a fourth shared adapter
package):

```go
type APIError struct {
    StatusCode int
    Type       string
    Code       string
    Param      string
    RetryAfter time.Duration
}
func (e *APIError) Error() string            // status + type/code/param only; never response text
func (e *APIError) Temporary() bool          // true for 429 or any 500..599
func (e *APIError) RetryDelay() time.Duration // parsed Retry-After, or 0

type ProtocolError struct { Code string }
func (e *ProtocolError) Error() string // event/code only
```

Parse REST/handshake error JSON `{"object":"error","message":"...","type":"...","param":"...","code":"..."}` from at most **64 KiB**. `code` may be a JSON string or number; normalize to
string. `message` and raw bodies are deliberately not retained or logged because they may echo
medical text. Parse `Retry-After` as integer seconds (HTTP-date parsing optional; if implemented,
test it). Other 5xx statuses are also temporary for classification, but `Temporary()` is not
permission to retry.

- 400/422: caller/model/options error; no retry.
- 401: missing/invalid/revoked key; 402: billing; 403: model/workspace permission; 404: model,
  voice, or endpoint. All fail clearly without exposing the key.
- 429 and 5xx: typed provider failure. **No automatic retry** in C2; no realtime replay or duplicate
  synthesis risk. C3/C5 can apply a policy using `Temporary`/`RetryAfter`.
- Context cancellation is wrapped/returned so `errors.Is(err, context.Canceled/DeadlineExceeded)`
  works. Transport errors wrap with operation (`stt dial`, `tts write`, `dialogue request`) but
  never URLs containing credentials (credentials are never in URLs anyway).
- Local `ErrStreamClosed`, `ErrTurnInProgress`, `ErrSessionFinished`, and `ErrTurnTruncated` are
  adapter sentinels for tests/callers; no new core sentinel is needed.

Logs use `log/slog`, snake_case keys, and nil logger → `io.Discard`. Allowed fields include
`order_id`, `model`, `operation`, `event_type`, `status_code`, `error_code`, `audio_bytes`,
`frames`, `dropped_chunks`, and `retry_after`. Never log API keys, auth headers, raw error bodies,
transcripts, prompt/assistant text, tool arguments/results, outcome detail, patient data, or raw
audio. Constructors and config summaries must not make network calls.

---

## 4. Layout and dependency law

Exactly three new adapter packages (all named `mistral`, aliased at composition-root imports):

```
internal/adapters/stt/mistral/
├── adapter.go          Config, New, Adapter, TranscribeStream/TranscribeFile entry points
├── realtime.go         WS handshake, feed writer, event reader, close lifecycle
├── batch.go            multipart request, response → domain.Transcript
├── wav.go              canonical PCM → 44-byte WAV header
├── language.go         BCP-47 primary-subtag → Mistral alpha-2
├── errors.go           APIError, ProtocolError, ErrStreamClosed
└── *_test.go           unit + fake realtime/batch integration

internal/adapters/tts/mistral/
├── adapter.go          Config, New, Adapter, Speak and mode selection
├── realtime.go         realtime SpeechStream, text queue, end/drain
├── offline.go          buffered-text SpeechStream, /speech SSE on Close
├── audio.go            float32le decode, 24→16 kHz polyphase converter, 640 B framing
├── sse.go              bounded stdlib SSE reader (TTS-local)
├── errors.go           APIError, ProtocolError, ErrStreamClosed
└── *_test.go           unit + fake realtime/offline integration

internal/adapters/dialogue/mistral/
├── adapter.go          Config, New, Adapter, NewSession validation/default prompt
├── protocol.go         request/message/tool JSON types and domain.Tool translation
├── session.go          committed history, pending calls, turn serialization, Close
├── stream.go           chat SSE reader, text deltas, fragmented tool-call assembly
├── outcome.go          finish_call, strict decode + semantic/domain validation
├── sse.go              bounded stdlib SSE reader (dialogue-local)
├── errors.go           APIError + turn/session sentinels
└── *_test.go           unit + fake chat integration
```

Modified files:

```
internal/core/config/config.go        Mistral nested fields, validation, Summary
internal/core/config/env.go          defaults normalization + 8 new registry entries
internal/core/config/config_test.go  config/default/validation/secret tests
internal/app/app.go                  pass shared Mistral config + logger to builders
internal/app/providers.go            three real cases with import aliases
internal/app/app_test.go             real construction, stub regression, no-startup-I/O
internal/app/mistral_live_test.go     new, //go:build live, double-gated and optional
Makefile                             live-test-mistral target; verify unchanged
README.md, docs/architecture.md, docs/mistral.md, PROGRESS.md
go.mod / go.sum                      unchanged (already contain coder/websocket)
```

**Untouched:** `internal/core/domain`, `internal/core/ports`, `internal/adapters/stub`, and the
Twilio package. **FLAG:** no port change is proposed or justified. If one appears necessary, stop
implementation and return to planning; do not silently broaden C2.

Import law remains:

```
internal/adapters/{stt,tts,dialogue}/mistral
    → internal/core/{ports,domain}
    → stdlib + github.com/coder/websocket[/wsjson] only
internal/app
    → core/{config,ports,domain} + all adapters (composition root only)
internal/core/config
    → stdlib only
```

Adapters do not import `internal/core/config`, `internal/app`, `internal/httpapi`, Twilio, or one
another. The small duplication of safe HTTP/SSE error helpers is deliberate; it is preferable to
a new shared adapter dependency for three protocols. Add compile-time assertions for every port
and stream/turn implementation.

Exact public construction surface (implementation details may remain unexported):

```go
// internal/adapters/stt/mistral
type Config struct {
    APIKey, BaseURL, RealtimeModel, BatchModel string
    StreamingDelayMS int
    HTTPClient *http.Client
    Logger *slog.Logger
}
func New(cfg Config) (*Adapter, error)

// internal/adapters/tts/mistral
type Config struct {
    APIKey, BaseURL, Model, DefaultVoice, Mode string
    HTTPClient *http.Client
    Logger *slog.Logger
}
func New(cfg Config) (*Adapter, error)

// internal/adapters/dialogue/mistral
type Config struct {
    APIKey, BaseURL, Model string
    HTTPClient *http.Client
    Logger *slog.Logger
}
func New(cfg Config) (*Adapter, error)
```

Port implementations use these exact existing signatures (in their respective packages):

```go
func (a *Adapter) TranscribeStream(context.Context, ports.StreamOptions) (ports.TranscriptionStream, error)
func (a *Adapter) TranscribeFile(context.Context, io.Reader, ports.BatchOptions) (domain.Transcript, error)
func (a *Adapter) Speak(context.Context, ports.SpeakOptions) (ports.SpeechStream, error)
func (a *Adapter) NewSession(context.Context, ports.SessionRequest) (ports.DialogueSession, error)

// Concrete stream/session/turn types implement, without signature changes:
Feed(context.Context, domain.AudioChunk) error
Results() <-chan ports.TranscriptionEvent
FeedText(context.Context, string) error
Audio() <-chan domain.AudioChunk
SendUserTurn(context.Context, string) (ports.AssistantTurn, error)
SubmitToolResults(context.Context, []ports.ToolResult) (ports.AssistantTurn, error)
TextChunks() <-chan string
Wait(context.Context) (ports.TurnResult, error)
Close(context.Context) error // on every stream/session; idempotent
```

Nil `HTTPClient`/`Logger` receive defaults. Empty direct-adapter model/base values receive the same
defaults as env config; empty API key is an error. Direct adapter construction permits `http` only
for a loopback hostname (`localhost`, `127.0.0.1`, or `::1`) so fake servers work without TLS;
non-loopback HTTP is rejected because it would expose the bearer key. The production env loader
requires HTTPS.

---

## 5. Config surface changes

### 5.1 Typed fields and defaults

Replace the single-field Mistral config with nested, shared settings (the existing
`Mistral.APIKey` field path remains `c.Mistral.APIKey`):

```go
type Mistral struct {
    APIKey   string // secret
    BaseURL  string
    STT      MistralSTT
    TTS      MistralTTS
    Dialogue MistralDialogue
}
type MistralSTT struct {
    RealtimeModel   string
    BatchModel      string
    StreamingDelayMS int
}
type MistralTTS struct {
    Model        string
    DefaultVoice string
    Mode         string // realtime | offline
}
type MistralDialogue struct { Model string }
```

Defaults:

```text
BaseURL                  https://api.mistral.ai
STT.RealtimeModel        voxtral-mini-transcribe-realtime-3
STT.BatchModel           voxtral-mini-transcribe-3
STT.StreamingDelayMS     480
TTS.Model                voxtral-mini-tts-3-0
TTS.DefaultVoice         voice_447
TTS.Mode                 realtime
Dialogue.Model           mistral-small-2603
```

`voxtral-small-transcribe-3` is an allowed operator-selected batch model for accuracy, but is not
the default. Model fields are non-empty compatible identifiers rather than hard-coded enums, so a
new Mistral release does not require a config-code change; protocol incompatibility remains a
live-test concern.

### 5.2 Environment registry (strict names)

Keep existing `INSTEADLY_MISTRAL_API_KEY` and add exactly these **8** entries:

| Variable | Default | Validation / meaning |
|---|---|---|
| `INSTEADLY_MISTRAL_API_BASE_URL` | `https://api.mistral.ai` | Absolute **https** origin, non-empty host, no userinfo, path (except `/`), query, or fragment; trim trailing `/` before validation |
| `INSTEADLY_MISTRAL_STT_REALTIME_MODEL` | `voxtral-mini-transcribe-realtime-3` | Non-empty after trim; no surrounding whitespace |
| `INSTEADLY_MISTRAL_STT_BATCH_MODEL` | `voxtral-mini-transcribe-3` | Same |
| `INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS` | `480` | Integer **80..1200 inclusive, or exactly 2400** |
| `INSTEADLY_MISTRAL_TTS_MODEL` | `voxtral-mini-tts-3-0` | Non-empty after trim; no surrounding whitespace |
| `INSTEADLY_MISTRAL_TTS_VOICE` | `voice_447` | Non-empty after trim; preset or workspace voice ID; not secret |
| `INSTEADLY_MISTRAL_TTS_MODE` | `realtime` | `realtime` or `offline` |
| `INSTEADLY_MISTRAL_DIALOGUE_MODEL` | `mistral-small-2603` | Non-empty after trim; no surrounding whitespace |

Reuse `strSetter`, `intSetter`, and `checkEnum`. Defaults apply even when all providers are stub,
so invalid explicitly supplied Mistral settings still fail loudly. If **any** of STT, TTS, or
Dialogue has provider `mistral`, require `strings.TrimSpace(APIKey) != ""`; when none is Mistral,
the key remains optional. Error messages name fields but never values. Strict rejection of every
unknown `INSTEADLY_*` variable, real-env > `.env` > defaults, and default stub providers remain
unchanged.

### 5.3 Key naming decision (explicit)

The app reads **only `INSTEADLY_MISTRAL_API_KEY`**, consistent with the C0/C1 invariant that
unprefixed variables are ignored. It must not add a `MISTRAL_API_KEY` fallback. The existing
unprefixed key is mirrored/exported by the user, never by application code:

```bash
export INSTEADLY_MISTRAL_API_KEY="$MISTRAL_API_KEY"
```

The build-tagged live test is the sole exception: like C1's `TEST_PHONE`, it reads test-only
`MISTRAL_API_KEY` directly with `os.Getenv`. It additionally requires `LIVE_MISTRAL=1`, so merely
having a key never triggers network/cost. Neither name's value may appear in test logs, config
summary, errors, screenshots, commits, or PROGRESS.

### 5.4 Secret-safe Summary

Add exactly these keys and update the exact-count test:

```go
"mistral.api_base_url":             c.Mistral.BaseURL,
"mistral.stt.realtime_model":       c.Mistral.STT.RealtimeModel,
"mistral.stt.batch_model":          c.Mistral.STT.BatchModel,
"mistral.stt.streaming_delay_ms":   c.Mistral.STT.StreamingDelayMS,
"mistral.tts.model":                c.Mistral.TTS.Model,
"mistral.tts.voice":                c.Mistral.TTS.DefaultVoice,
"mistral.tts.mode":                 c.Mistral.TTS.Mode,
"mistral.dialogue.model":           c.Mistral.Dialogue.Model,
```

Never add `mistral.api_key` or a redacted key fingerprint. Existing provider keys remain;
`/healthz` still exposes only provider names, not these model settings.

---

## 6. Implementation steps (do them in this order)

### Step 0 — Preconditions and dependency freeze

1. Run the baseline commands in §2 and re-read current Mistral docs if they changed since
   2026-11-17. Do not edit `.env`.
2. Confirm `go list -m all` contains only the main module and `github.com/coder/websocket
   v1.8.15`; no `go get` is needed.
3. Verify client support from the installed package docs:
   ```bash
   go doc github.com/coder/websocket.Dial
   go doc github.com/coder/websocket.DialOptions
   ```
4. Verify: `go test -count=1 ./...` remains green before the first edit.

### Step 1 — Config surface

1. Add nested fields/defaults, eight registry entries, URL normalization/validation, delay/mode/
   model validation, and conditional API-key requirement from §5.
2. Add the eight Summary keys. Keep all provider enums and strict unknown-name behavior unchanged.
3. Update config tests before moving on: every default, valid override, malformed URL, missing key
   per provider, key optional for all-stub, delay boundaries (79, 80, 1200, 1201, 2400, 2401),
   mode, whitespace/empty model/voice, strict unknown var, and no secret values/keys.
4. Verify:
   ```bash
   go test -race -count=1 ./internal/core/config
   go build ./...
   ```

### Step 2 — STT pure helpers and batch adapter

1. Add `stt/mistral.Config/New`, safe errors, language normalization, WAV writer, batch multipart
   request, and response mapping from §3.2/§3.3.
2. New validates but performs no network. Use `httptest.NewServer` for every batch test; assert
   auth without printing the fake key, multipart fields, WAV header, exact PCM payload, and all
   error/overflow/segment cases in §7.
3. Verify:
   ```bash
   go test -race -count=1 ./internal/adapters/stt/mistral
   ```

### Step 3 — Realtime STT stream

1. Implement handshake created→update→updated, copied/bounded audio queue, base64 append writer,
   cumulative partial reader, final done handling, bounded results, and idempotent draining Close.
2. Build a fake Mistral WebSocket with `httptest` + `websocket.Accept`; the production adapter uses
   `websocket.Dial` with `DialOptions.HTTPHeader` and the injected client. No real network.
3. Test full audio/text flow, close flushing, concurrent feeds/close, drop-oldest, malformed events,
   error event, handshake errors, cancellation, and goroutine/channel cleanup under `-race`.
4. Verify: `go test -race -count=1 ./internal/adapters/stt/mistral`.

### Step 4 — TTS audio pipeline

1. Implement float32le decode/validation, streaming 15-tap anti-alias + 3:2 polyphase resampling,
   int16le conversion, tail flush, and 640-byte framer exactly as §3.4.
2. Table-test known normalized values, clipping, arbitrary byte boundaries, 24 kHz→16 kHz sample
   counts, all output lengths, final padding, and tone quality. Include a 10 kHz input tone showing
   attenuation relative to 1 kHz, and a known short reference vector so a phase error cannot pass.
3. Verify: `go test -race -count=1 ./internal/adapters/tts/mistral -run 'TestAudio|TestPCM|TestResample'`.

### Step 5 — Realtime and offline TTS streams

1. Implement realtime created/update/updated, ordered text queue, audio delta conversion, end/drain,
   and error lifecycle. One port stream is one generation; do not add flush/generation APIs.
2. Implement offline buffered-text stream and its `/v1/audio/speech` `stream=true` SSE request on
   Close, sharing the audio pipeline and local SSE reader.
3. Fake both protocols. Assert exact JSON/messages, voice override, no automatic punctuation flush,
   no offline request before Close, canonical output across split base64 deltas, and all error/
   cancellation/idempotence cases.
4. Verify: `go test -race -count=1 ./internal/adapters/tts/mistral`.

### Step 6 — Dialogue protocol, SSE, and history

1. Implement config/NewSession validation, tool translation, reserved tool insertion, committed
   history, one-turn-at-a-time state, and Chat Completions request.
2. Implement bounded SSE, text/content-part deltas, tool-call fragment assembly by index, terminal
   reasons, Wait semantics, exact tool-result continuation, and rollback on failed turns.
3. Do not execute tools and do not loop over successive calls automatically.
4. Fake the server with `httptest`; assert complete second-request history after tool results,
   auth/model/options, streamed text ordering, all malformed/error/cancellation cases, and races.
5. Verify: `go test -race -count=1 ./internal/adapters/dialogue/mistral`.

### Step 7 — Outcome contract and validation

1. Add reserved `finish_call` interception, strict `domain.Outcome` decode, §3.8 conditional
   checks, throwaway `Order.Finish` validation, and terminal session state.
2. Unit-test every outcome kind and every missing/irrelevant/malformed field. Assert outcome is not
   also returned as a normal tool call and cannot be combined with one.
3. Put the exact §3.7/§3.8 tool and outcome contract in `docs/mistral.md`; do not create C3 tool
   handlers or a core agent package.
4. Verify: `go test -race -count=1 ./internal/adapters/dialogue/mistral`.

### Step 8 — Composition-root wiring

1. Change builder calls to receive `cfg.Mistral` and `log`; use import aliases `sttmistral`,
   `ttsmistral`, and `dialoguemistral`. Map fields one-for-one into adapter configs.
2. Replace only the three `ErrNotImplemented` branches. Stub and Twilio behavior stays unchanged.
3. App tests construct each and all Mistral providers with a fake key and `https://example.test`;
   assert non-nil ports and make no calls. Constructor-time network I/O is forbidden, so these
   tests need no fake server. Missing-key rejection belongs to config tests.
4. Verify:
   ```bash
   go test -race -count=1 ./internal/app
   go test -race -count=1 ./...
   /Library/Developer/CommandLineTools/usr/bin/make check-deps
   ```

### Step 9 — Full fake-provider verification

1. Add one integration test per adapter covering the complete port sequence, not just JSON helper
   functions. Run all tests with race detection repeatedly to expose close/send races:
   ```bash
   go test -race -count=10 ./internal/adapters/stt/mistral ./internal/adapters/tts/mistral ./internal/adapters/dialogue/mistral
   go test -race -count=1 ./...
   ```
2. Confirm tests pass with real outbound network disabled/no real key. Fake servers must bind only
   loopback. Assert fake secret does not occur in captured logs/errors.
3. Run `go mod tidy` and verify `git diff --exit-code -- go.mod go.sum`: no dependency drift.

### Step 10 — Optional live smoke implementation and docs

1. Add `internal/app/mistral_live_test.go` with `//go:build live`; it skips unless both
   `LIVE_MISTRAL=1` and non-empty `MISTRAL_API_KEY` are present. Read the latter directly in the
   test, never via app config. Do not print it.
2. Keep the smoke cheap/idempotent and sequential:
   - **TTS:** synthesize `Guten Tag.` in realtime, assert non-empty 640-byte canonical frames;
   - **STT:** feed those frames to realtime STT at roughly 20 ms pacing, call Close, require a final
     transcript and case-insensitively check `guten` (allow provider punctuation/case differences);
   - **Dialogue:** offer only `capture_info`, ask explicitly to capture reason `Kontrolle`, drain
     text, and require one `capture_info` call with valid arguments; **do not execute it** or submit
     a result. A model refusal/wrong call is a live failure to investigate, not something to hide
     with retries.
3. Add `make live-test-mistral` for exactly:
   `go test -tags live -count=1 -run '^TestLiveMistral$' -v ./internal/app`.
   Do not include it in `verify`, CI, or the Twilio live target. Running it costs a small metered
   amount and requires explicit user approval.
4. Document setup, exact protocols, audio conversion, TTS offline latency, error/retry ownership,
   tool/outcome contract, key mirroring, and the live recipe in `docs/mistral.md`; update README
   config/dependencies/status and architecture extension points. `docs/api.md` needs no route
   change because Mistral is outbound-only.
5. Compile/skip-check without running live:
   ```bash
   go test -tags live -count=1 -run '^TestLiveMistral$' -v ./internal/app
   # expected: SKIP unless LIVE_MISTRAL=1 and MISTRAL_API_KEY are both set
   ```

### Step 11 — Exit verification and PROGRESS

1. Run `/Library/Developer/CommandLineTools/usr/bin/make verify` and `make cover` with the working
   CommandLineTools path if the default shim remains broken.
2. Run the boot matrix in §8, check no core domain/port diff, no dependency diff, no secrets in
   logs, and no unexpected files.
3. If (and only if) the user approves the optional live run, execute:
   ```bash
   LIVE_MISTRAL=1 MISTRAL_API_KEY="$MISTRAL_API_KEY" \
     /Library/Developer/CommandLineTools/usr/bin/make live-test-mistral
   ```
   Record only outcomes/models/latencies, never text containing patient data or the key. A live
   protocol mismatch gets an Amendments entry and must be fixed before C3 relies on it.
4. Update PROGRESS with exact observed commands/results, C2 status, live result or "not run
   (optional)", environment key **names only**, and any amendments. Do not commit unless asked.

---

## 7. Test plan (file → cases)

All external-provider tests use `httptest` HTTP/SSE and `coder/websocket` fake servers. They use a
known fake key, require **no credentials and no non-loopback network**, and never call Mistral.
Only `mistral_live_test.go` has the `live` tag.

| Test file | Cases |
|---|---|
| `config/config_test.go` | all Mistral defaults; eight env overrides; base URL https/host/no path/query/userinfo/fragment and slash trimming; delay 80/1200/2400 accepted, 79/1201/2401/non-int rejected; mode enum; empty/whitespace model/voice rejected; API key required independently for each Mistral provider and once for all three; all-stub key optional; unknown var still rejected; exact Summary key list/count and fake key absent from keys/values |
| `stt/mistral/language_test.go` | empty, `de`, `DE`, `de-AT`, `en-US`; malformed, 1-letter, 3-letter, non-ASCII rejected before dial |
| `stt/mistral/wav_test.go` | exact 44-byte PCM WAV fields (RIFF sizes, 16 kHz, 32 kB/s, mono, 16-bit); payload byte equality; empty/odd/overflow input |
| `stt/mistral/batch_test.go` | multipart auth/model/file/language/repeated hints; timestamps only; diarization implies segment; response text/segments/durations/null speaker; no timestamps → empty segments; malformed/negative/reversed times; 400/401/402/404/422/429/500 `APIError`, numeric/string code, Retry-After; oversized/malformed response; reader error; context cancellation |
| `stt/mistral/realtime_test.go` | query/auth; created/update/updated ordering; exact pcm_s16le/16k/480/language/hints and no turn_detection; sequential Feed preserves exact bytes and concurrent Feed is race-safe; delta→cumulative partials; done→one final; empty final; end sent once on Close; results/audio channel close; repeated/concurrent Close; Feed after Close; odd/empty chunks; bounded queue drop-oldest + warning count; partial overflow never drops final; malformed known event/error event/abnormal close; setup rejection; close timeout/cancel; fake key absent from logs/errors |
| `stt/mistral/integration_test.go` | one fake realtime utterance from arbitrary canonical chunks through final; one batch recording through `domain.Transcript`; compile-time port assertions; no goroutine leak after repeated sessions under `-race` |
| `tts/mistral/audio_test.go` | float32le known samples (-1/0/+1), endianness, NaN/Inf and non-4-byte rejection; clipping/int16 extrema; arbitrary delta boundaries; 480 input samples→320 output samples; every frame 640 B; tail flush and one final pad only; known polyphase vector/phase continuity; 10 kHz attenuation vs 1 kHz; long-stream bounded state |
| `tts/mistral/realtime_test.go` | WS query/auth; exact responsive/pcm/voice update; SpeakOptions voice override; appends preserve spaces/no auto-flush; empty and invalid UTF-8 handling; split audio deltas → canonical frames; done flush before close; input_text.end once; Audio closed; Close idempotent/concurrent; FeedText after close; queue/context backpressure; malformed base64/audio/JSON/error event/abnormal close; handshake errors; final timeout; no secret/text/audio in logs |
| `tts/mistral/offline_test.go` | Speak/FeedText make no request; Close sends one exact JSON request with joined text/voice/pcm/stream=true; empty input; SSE CRLF/comments/multiline data; audio delta conversion/done; non-2xx and malformed SSE; cancellation; repeated Close does not POST twice |
| `tts/mistral/integration_test.go` | realtime text deltas→audio→640 B canonical output; offline same audio pipeline; no realtime→offline fallback on injected failure; port assertions and race cleanup |
| `dialogue/mistral/protocol_test.go` | tool schema raw round-trip; name validation/duplicates/reserved finish_call; invalid/non-object parameters/outcome schema; request model/stream/temp/max tokens/tool_choice/parallel false; content string and text-part decoding; safe APIError parsing/classification |
| `dialogue/mistral/stream_test.go` | streamed text exact ordering/whitespace; TextChunks closes before Wait ready; `[DONE]`; fragmented tool arguments by index with omitted later id/name; multiple sequential calls; compact valid object args; duplicate/missing IDs, noncontiguous index, unknown name, malformed/trailing/non-object args, oversized text/args/event/call count; finish_reason stop/tool_calls/length; premature EOF; malformed SSE; Wait cancellation |
| `dialogue/mistral/session_test.go` | NewSession no HTTP; default/override prompt plus terminal instruction; user-turn request history; successful history commit and failure rollback; blank input; exact pending-ID tool results and role/name/content/tool_call_id order; unknown/missing/duplicate IDs and invalid result JSON; one in-flight turn; Close cancels; repeated Close; no automatic tool execution/loop |
| `dialogue/mistral/outcome_test.go` | booked/callback_needed/failed/aborted valid mapping; RFC3339 times; each kind's missing required and irrelevant fields; blank detail; unknown kind/field; zero/malformed time; finish_call omitted from ToolCalls; combination with ordinary call rejected; session finished afterward; `domain.ErrInvalidOutcome` wrapping where applicable |
| `dialogue/mistral/integration_test.go` | fake user turn→text; next turn→tool call; SubmitToolResults→assistant continuation with exact full history; separate finish_call→Outcome; HTTP 401/429/500 and context cancellation; no credentials/network beyond loopback |
| `app/app_test.go` | each Mistral provider and all three wire to non-nil ports with fake key/base URL; constructors make no network; stubs unchanged; Twilio tests unchanged; unknown providers still error |
| `app/mistral_live_test.go` (`live`) | double gate skips by default; when explicitly enabled, short TTS→canonical audio, STT final containing `guten`, dialogue capture_info tool call; no tool execution and no secret logging; optional, not CI/verify |
| `httpapi/server_test.go` | existing healthz provider names reflect configured Mistral providers without exposing config/key; all prior routes/webhook behavior unchanged |

Run `make test` (`-race`), `make lint`, `make check-deps`, `make build`; all green. Aim for
**≥80 % statements in each new adapter package**, with protocol state machines and audio conversion
covered more heavily than getters; overall project coverage must not regress materially from C1.

---

## 8. Definition of done (exit criteria — record evidence in PROGRESS)

1. **Build/test:** `/Library/Developer/CommandLineTools/usr/bin/make verify` is green (gofmt,
   `go vet`, optional golangci-lint when installed, `go test -race -count=1 ./...`,
   `make check-deps` → `OK: core imports no adapters`, binary builds).
2. **No contract drift:** `internal/core/domain` and `internal/core/ports` have no diff from the
   C2 base; all three adapters satisfy the exact existing interfaces. `git diff <base> --
   internal/core/domain internal/core/ports` is empty.
3. **Dependency freeze:** `go.mod` still has exactly one third-party require,
   `github.com/coder/websocket v1.8.15`; `go mod tidy` produces no `go.mod`/`go.sum` diff.
4. **Credential-free protocol proof:** the §7 fake-server suites pass, including a full realtime
   STT stream/final, batch multipart+WAV/transcript, realtime and offline TTS to canonical audio,
   dialogue streaming/tool-result continuation, and validated outcomes. The repeated race command
   in Step 9 passes.
5. **Canonical audio evidence:** STT sends byte-identical canonical PCM16/16 kHz; every TTS test
   output chunk is 640-byte PCM16le/16 kHz; tests prove 24 kHz float32 conversion, anti-alias
   behavior, arbitrary chunk boundaries, and final flush/padding. No Mistral format escapes a port.
6. **Lifecycle/error evidence:** close/feed/send races are clean under `-race`; final channels close;
   no automatic retries/fallbacks; 4xx/429/5xx and WS errors produce safe typed errors; fake key,
   prompt, transcript, tool args, and raw provider error body do not appear in captured logs.
7. **Config/boot matrix:**
   - no env → all-stub binary still boots and healthz is unchanged;
   - each/all `stt|tts|dialogue=mistral` with `INSTEADLY_MISTRAL_API_KEY=test-key` and valid
     defaults → app/binary boots without contacting `api.mistral.ai`, healthz names `mistral`;
   - any Mistral provider with missing key → exit 1 `config: mistral.api_key ... required`;
   - invalid base URL, delay, TTS mode, or empty model/voice → exit 1 naming the field;
   - `INSTEADLY_MISTRAL_TYPO=1` → strict unknown-variable error;
   - unprefixed `MISTRAL_API_KEY` alone does not satisfy app config.
8. **Tool/outcome resolution:** docs contain §3.7's exact five-tool surface and §3.8 schema;
   fake dialogue tests cover all four domain outcome kinds. No agent loop/tool handler/order
   mutation was added.
9. **Live smoke is optional, not an exit gate:** the build-tagged test compiles and is observed to
   `SKIP` without both gates. If the user authorizes a live run, record its exact result and any
   protocol amendment; C2 may be ✅ on the fake proof with live explicitly recorded "not run".
   C3 must not claim real M0 verification until the live chain has actually passed.
10. **Docs/hygiene:** README, architecture, and `docs/mistral.md` are current; Makefile live target
    is excluded from verify; PROGRESS updated per protocol; `.env` untouched, no secret/key value
    or patient text printed, no commit unless asked, `git status` contains only expected files.

---

## 9. Risks and decision gates

| Risk | Mitigation / gate |
|---|---|
| Raw `pcm` sample encoding differs from the float32le integration evidence | Exact encoding is pinned in §2.7/§3.4 and unit-tested; **live protocol gate:** short TTS smoke must produce intelligible PCM after conversion. If Mistral returns another encoding, stop live reliance, record an amendment with evidence, and update only the TTS adapter; never reinterpret bytes silently |
| 24→16 kHz resampling aliases, clicks at delta boundaries, or loses tail | Stateful polyphase/filter tests, known vectors, arbitrary splits, 10 kHz attenuation, tail flush; do not use naïve every-third-sample decimation |
| STT caller expects a final per utterance but never closes input | Adapter contract says Close=end-of-input and waits for done; C3 usage note: normally one stream per core-VAD utterance. C3 latency gate measures reconnect/setup overhead before choosing a persistent-session optimization |
| Enabling Mistral turn detection would violate core-owned VAD or lose late text | C2 never enables it and exposes no turn events. Any future hint-only use requires a separate design review, not an opportunistic config flag |
| Bounded STT queues drop audio and corrupt transcript | Required drop counter + aggregate warning; 100-frame buffer; tests. C3 treats drops as degraded transcription and may fail/retry rather than booking from incomplete text |
| TTS realtime `Close` deadlocks waiting for done or consumer | Context + 30 s cap, cancellation closes socket, single channel owner, race/repeat tests. Offline mode's no-audio-before-Close behavior is explicit, never a silent fallback |
| SSE/WS fragmentation (tool args, base64, multiline data) parsed incorrectly | Stateful assemblers by tool index, bounded stdlib SSE parser, fragmented fake events, malformed-input tests |
| Model emits unknown/malformed tool or invalid outcome | Allowlist names, strict object/unknown-field decode, domain + conditional validation; never execute or persist automatically. `finish_call` is exclusive and terminal |
| Failed dialogue turn corrupts history or retries duplicate speech | Candidate-history commit only on success; no automatic retry; partial text already streamed is documented and C3 aborts a failed turn/call |
| Model alias changes booking behavior | Default dated `mistral-small-2603`; override is explicit; live smoke includes a real function call. German quality/latency is a C3 evaluation gate |
| Provider rate limits/outages | Typed `Temporary`/`RetryAfter`, no hidden retry; C3/C5 owns backoff. A 401/402/403/404 is surfaced as configuration/billing/access, not retried |
| Sensitive medical data or key leaks via logs/errors | No payload/error-message logging, secret-safe Summary, fake-secret assertions, no adapter persistence; production compliance remains the existing later gate |
| Optional live smoke costs money or runs accidentally | Requires both `LIVE_MISTRAL=1` and key, build tag, explicit command/user approval; excluded from CI/verify; three short idempotent calls and no side-effecting tool execution |
| C1 live audio remains blocked on Twilio trial | C2 tests are independent and need no telephony. Do not claim an end-to-end phone call; C3 still needs the Twilio upgrade plus real Mistral verification |

---

## 10. Expected size & effort

Roughly **2,000–2,600 lines** of new adapter code and tests, plus ~350 lines of config, wiring,
docs, and live-smoke changes. Plan **three focused sessions**:

1. config + STT batch/realtime + fake protocols;
2. TTS audio conversion + realtime/offline streams + fake protocols;
3. dialogue SSE/history/tools/outcomes + wiring, race runs, docs, optional live smoke.

The slow parts are stream shutdown/ownership, fragmented tool-call assembly, and correct stateful
audio resampling — not the HTTP boilerplate. Copy protocol shapes from §3 and the cited docs, not
from memory. Keeping the existing ports and avoiding new dependencies saves substantial scope.

---

## 11. What C3 plugs into here

- **Inbound voice:** C3's core VAD consumes Twilio `CallSession.Inbound()` canonical frames and
  feeds a `ports.TranscriptionStream`. Normally it opens one STT stream for an utterance, feeds
  the utterance, drains cumulative partials as needed, then calls `Close` to obtain exactly one
  final transcript. Close is the generic end-of-input contract, not a Mistral type in the core.
- **Dialogue:** C3 creates `SessionRequest{OrderID, SystemPrompt, Tools, OutcomeSchema}` using the
  order/patient context, §3.7 tools, and §3.8 schema. It drains `TextChunks`, calls `Wait`, executes
  only authorized ordinary tool calls, submits their JSON results once, and repeats under its own
  loop/policy. It never executes `finish_call`; a non-nil `TurnResult.Outcome` is validated input
  for the real order state machine.
- **Outbound voice:** C3 opens one realtime `SpeechStream` per assistant generation, forwards each
  dialogue text delta with `FeedText`, and runs an `Audio()` drainer that forwards every 640-byte
  chunk to `CallSession.SendAudio`. At end-of-input it calls TTS `Close` while that drainer remains
  active, then waits for the closed audio channel. Twilio still owns 16→8 kHz and µ-law; C3 sees
  only canonical PCM.
- **Post-call:** C3/storage policy may call `TranscribeFile` on retained canonical recording PCM
  with timestamps/diarization/hints and persist the returned `domain.Transcript` only when policy
  allows. The STT adapter itself stores nothing.
- **Outcomes:** C3 (and C5 callback scheduling) consumes `booked`, `callback_needed`, `failed`, and
  `aborted`. `callback_at` drives later rescheduling; `fail_reason` drives policy. C2 only parses
  and validates the model result.
- **Operations:** C3/C5 may use `errors.As` with locally declared interfaces
  `interface{ Temporary() bool }` and `interface{ RetryDelay() time.Duration }` for deliberate
  retry policy; they must not import an adapter to reach concrete `APIError` fields. C4 owns
  barge-in/cancel-versus-flush semantics and any future Mistral turn-hint decision. C7 can add
  another provider behind the unchanged ports.
- **M0 gate:** with user approval, C3 combines the already-live-tested adapters, upgraded Twilio
  Media Streams, and the real key into the first actual phone conversation. C2's fake tests alone
  are not that evidence.

---

## 12. Sources consulted (2026-11-17)

Primary Mistral protocol/model documentation actually fetched:

- Realtime STT lifecycle, endpoint, auth, session fields, audio/text/done/error messages:
  <https://docs.mistral.ai/studio/audio/speech-to-text/transcribe-in-realtime>
- STT turn detection and its late-text behavior (basis for leaving it off):
  <https://docs.mistral.ai/studio/audio/speech-to-text/turn-detection>
- File transcription multipart fields, model names, segment response, diarization/timestamps:
  <https://docs.mistral.ai/studio/audio/speech-to-text/transcribe-files>
- Context-bias fields in realtime JSON and repeated multipart form fields:
  <https://docs.mistral.ai/studio/audio/speech-to-text/bias-context-and-vocabulary>
- STT model selection: <https://docs.mistral.ai/studio/audio/speech-to-text/overview>
- Realtime TTS endpoint, created/update/append/end, audio delta/done/error, and generation rules:
  <https://docs.mistral.ai/studio/audio/text-to-speech/speak-in-realtime>
- Offline `/v1/audio/speech` JSON request and `audio_data` response:
  <https://docs.mistral.ai/studio/audio/text-to-speech/generate-speech>
- Offline streamed SSE response with `stream=true`:
  <https://docs.mistral.ai/studio/audio/text-to-speech/stream-speech>
- TTS model/mode lineup: <https://docs.mistral.ai/studio/audio/text-to-speech/overview>
- Audio formats: raw `pcm`, generated speech mono 24 kHz, realtime STT pcm_s16le default 16 kHz:
  <https://docs.mistral.ai/studio/audio/specs-and-limits/audio-formats>
- Audio limits, including streaming-delay values and rate-limit scope:
  <https://docs.mistral.ai/studio/audio/specs-and-limits/limits>
- Chat Completions HTTP/SSE and multi-turn messages:
  <https://docs.mistral.ai/inference/conversations/chat-completion>
- Native Mistral function-tool schema, tool calls/results, tool choice, parallel setting:
  <https://docs.mistral.ai/inference/conversations/function-calling>
- Custom JSON-schema response format (considered; reserved function chosen instead):
  <https://docs.mistral.ai/inference/conversations/structured-output/custom>
- Mistral Small 4 dated API name, GA status, function-calling/structured-output support:
  <https://docs.mistral.ai/models/mistral-small-4-0-26-03>
- Mistral status/error JSON and transient-status guidance:
  <https://docs.mistral.ai/resources/error-glossary>

Additional implementation references actually fetched:

- **Float32le PCM encoding evidence:** Pipecat's Mistral TTS integration documents 24 kHz PCM and
  its `_float32_to_int16` conversion of the base64 `pcm` payload (official Mistral audio-format
  docs specify mono/rate/container but not the raw PCM scalar type):
  <https://reference-server.pipecat.ai/en/stable/_modules/pipecat/services/mistral/tts.html>
- Mistral TypeScript generated `SpeechStreamAudioDelta` (`audio_data` wire field):
  <https://raw.githubusercontent.com/mistralai/client-ts/main/src/models/components/speechstreamaudiodelta.ts>
- `coder/websocket` v1.8.15 client `Dial`, `DialOptions` headers/client, Accept, read limits:
  <https://pkg.go.dev/github.com/coder/websocket@v1.8.15>
- Micdrop's Mistral integration (corroborating utterance-end/final-transcript usage and 16 kHz
  canonical conversion; not used as protocol authority over Mistral docs):
  <https://micdrop.dev/docs/ai-integration/provided-integrations/mistral>

---

## 13. Amendments

_None yet. This plan becomes read-only when C2 implementation starts. Any necessary deviation
must be recorded here with date, evidence, and rationale, and mirrored in the PROGRESS session
log; do not silently rewrite the contract being implemented._

### 2026-11-17 — conductor approval & overrides

Approved for implementation by the conductor (multi-agent run).

1. **Commit/push/PR authorized.** The plan's "do not commit unless asked" clauses are overridden:
   the user explicitly authorized autonomous work with pull requests. The Coder works on
   `feat/c2-mistral-ai`, pushes, and opens one PR against `main`.
2. **Live smoke authorized.** A real `MISTRAL_API_KEY` is present in `.env` (unprefixed) and the
   user authorized using it. After the fake tests are green, the Coder runs the optional live smoke
   (Step 10/11.3). A protocol mismatch (especially the float32 PCM encoding) is a MUST-FIX
   amendment, never a silent pass; record it here and fix the adapter before marking C2 done.

### 2026-11-17 — C2 implementation session (Europe/Vienna)

Live smoke **PASSED** (TTS → 40 canonical 640-byte frames → STT final transcript → one dialogue
`capture_info` tool call). The plan's #1 risk — the raw `pcm` float32 LE mono 24 kHz encoding —
was **confirmed correct on the first live attempt**; no encoding amendment was needed.

Two close-frame robustness amendments were required (both fixed and re-verified green):

1. **TTS realtime close-after-done.** Mistral closes the realtime TTS WebSocket without a clean
   `1000` close frame after `speech.audio.done` (`websocket.CloseStatus == -1`). The reader now
   treats *any* read error/close after `speech.audio.done` as a successful end. Rationale: the
   generation is already complete; failing it would discard valid audio. Before `speech.audio.done`,
   an abnormal close still fails the stream as specified.
2. **STT realtime close-after-done.** The realtime STT WebSocket likewise closes without a clean
   `1000` after `transcription.done`. The reader now treats *any* read error/close after
   `transcription.done` as success (the final transcript was already emitted). Before
   `transcription.done`, a clean close still yields `final transcript missing` and an abnormal close
   still yields a protocol error, exactly as specified.

These deviate from §3.2/§3.4's "normal close (1000)" wording only; they are transport
robustness, not a PCM/model/message-shape mismatch. The float32 encoding, model names, and JSON
message shapes all matched the plan verbatim.
