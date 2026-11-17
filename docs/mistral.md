# Mistral adapters (C2)

Insteadly's speech-to-text, text-to-speech, and dialogue providers are implemented with the
Mistral AI platform's raw HTTP/SSE/WebSocket APIs. There is no Go SDK; these adapters speak
the documented wire protocols directly and never let a Mistral concept (model names, base64
payloads, SSE frames, 24 kHz float32 PCM) cross a port boundary.

Packages (each named `mistral`, aliased at the composition root):

```
internal/adapters/stt/mistral/       realtime + batch transcription
internal/adapters/tts/mistral/       realtime + offline synthesis, 24→16 kHz conversion
internal/adapters/dialogue/mistral/  chat completions, tools, finish_call outcome
```

## Configuration

All settings use the `INSTEADLY_MISTRAL_*` prefix. Precedence is process environment > `.env` >
defaults; unknown `INSTEADLY_*` names are a startup error. The app reads **only**
`INSTEADLY_MISTRAL_API_KEY` — the unprefixed `MISTRAL_API_KEY` in `.env` is ignored by the app
and must be mirrored by the operator:

```bash
export INSTEADLY_MISTRAL_API_KEY="$MISTRAL_API_KEY"
```

| Variable | Default | Notes |
|---|---|---|
| `INSTEADLY_MISTRAL_API_KEY` | — | **secret**; required only when a Mistral provider is selected |
| `INSTEADLY_MISTRAL_API_BASE_URL` | `https://api.mistral.ai` | absolute **https** origin, no path/query/fragment |
| `INSTEADLY_MISTRAL_STT_REALTIME_MODEL` | `voxtral-mini-transcribe-realtime-3` | |
| `INSTEADLY_MISTRAL_STT_BATCH_MODEL` | `voxtral-mini-transcribe-3` | |
| `INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS` | `480` | 80..1200, or exactly 2400 |
| `INSTEADLY_MISTRAL_TTS_MODEL` | `voxtral-mini-tts-3-0` | |
| `INSTEADLY_MISTRAL_TTS_VOICE` | `voice_447` | preset or workspace voice id |
| `INSTEADLY_MISTRAL_TTS_MODE` | `realtime` | `realtime` or `offline` |
| `INSTEADLY_MISTRAL_DIALOGUE_MODEL` | `mistral-small-2603` | dated API name for Mistral Small 4 |

Select a provider with `INSTEADLY_STT_PROVIDER=mistral`, `INSTEADLY_TTS_PROVIDER=mistral`, and/or
`INSTEADLY_DIALOGUE_PROVIDER=mistral`. Model fields are compatible identifiers, not hard-coded
enums, so a new Mistral release does not require a code change (protocol incompatibility remains
a live-test concern).

## Endpoints

Production base URL `https://api.mistral.ai`; every request and WebSocket handshake sends
`Authorization: Bearer <key>`. WebSocket URLs swap the scheme (`https`→`wss`, `http`→`ws` only in
loopback tests) and carry only the documented `model` query.

| Capability | Endpoint |
|---|---|
| Realtime STT | `wss://api.mistral.ai/v1/audio/transcriptions/realtime?model=voxtral-mini-transcribe-realtime-3` |
| Batch STT | `POST /v1/audio/transcriptions` (multipart) |
| Realtime TTS | `wss://api.mistral.ai/v1/audio/speech/realtime?model=voxtral-mini-tts-3-0` |
| Offline TTS | `POST /v1/audio/speech` (`stream=true`, SSE) |
| Dialogue | `POST /v1/chat/completions` (SSE) |

WebSocket sessions follow `session.created` → `session.update` → `session.updated` before any
audio/text is accepted; invalid model/options fail at open, not later.

## Audio conversion (TTS)

Mistral synthesizes speech as **float32 little-endian, mono, 24 kHz** (`response_format="pcm"`).
The canonical core format is **PCM16 little-endian, mono, 16 kHz** (640 bytes = 20 ms), so every
output chunk passes through a streaming **15-tap Hamming-windowed sinc low-pass** (cutoff 7.2 kHz)
followed by 3:2 polyphase decimation with linear interpolation at half-sample phases. The filter
state, phase, and history persist across arbitrary WebSocket/SSE delta boundaries; the tail is
flushed on `speech.audio.done` and a residual frame is zero-padded to exactly 640 bytes. Every
`Audio()` value has `len == 640`. This was verified live (see below).

## TTS modes

- **`realtime`** (default) speaks text as it is generated: `FeedText` enqueues
  `input_text.append` messages and audio deltas are converted as they arrive.
- **`offline`** buffers text and sends one `POST /v1/audio/speech` (`stream=true`) on `Close`, so
  **no audio can arrive before `Close`**. This latency is intentional and is never a silent
  fallback — a realtime failure surfaces as an error (a retry/fallback could speak text twice).

Both modes share the same `ports.SpeechStream` and the same conversion pipeline.

## Dialogue: tools and the outcome

The dialogue adapter is generic over `SessionRequest.Tools` and appends one reserved
`finish_call` tool whose `parameters` is the caller's `OutcomeSchema`. The canonical tool surface
for C3 (defined here as the contract, not implemented — C2 executes nothing):

| Tool | Argument properties | Required | Handler-result contract |
|---|---|---|---|
| `propose_slot` | `start` (date-time), `end` (date-time), `doctor` (string), `practice` (string) | `start`, `end` | Propose, never book. C3 checks `start < end` and preference policy. Result: `{"ok":true,"start":"…","end":"…"}` or `{"ok":false,"error":"outside_preferences"}` |
| `confirm_booking` | same four plus `confirmation_code` (string) | `start`, `end` | Record a slot only after explicit clinic acceptance. Result: `{"ok":true,"confirmation_code":"…"}` or `{"ok":false,"error":"not_confirmed"}`. Does not replace terminal `finish_call` |
| `capture_info` | `field` (enum `full_name`, `phone`, `birth_date`, `insurance_number`, `reason`, `note`), `value` (string, 1..2048 chars) | `field`, `value` | Capture one user-authorized field; no arbitrary keys. Result: `{"ok":true,"field":"reason"}` or `{"ok":false,"error":"invalid_value"}` |
| `escalate_to_human` | `reason` (non-empty string), `urgent` (boolean) | `reason` | Ask C3 to hand off; C2 never performs a transfer. Result: `{"ok":true,"action":"escalate_to_human"}` or `{"ok":false,"error":"unavailable"}` |
| `finish_call` | the canonical outcome schema below | per schema | Reserved terminal outcome, intercepted by the dialogue adapter; no C3 tool result |

Descriptions must state the side-effect rules above, especially that `propose_slot` is not
a booking and `confirm_booking` requires explicit acceptance. `capture_insurance` is
deliberately not a separate tool: `capture_info(field="insurance_number")` covers it without
expanding the surface. DTMF, hold, and barge-in tools are C4 decisions, not hidden C2
additions.

Every schema is `{"type":"object", "additionalProperties":false}`; timestamps are RFC 3339
`format:"date-time"`. The canonical outcome schema (passed as `SessionRequest.OutcomeSchema`):

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "kind": {"type": "string", "enum": ["booked", "callback_needed", "failed", "aborted"]},
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

Conditional rules are enforced in Go (blank detail everywhere; `booked` forbids
`callback_at`/`fail_reason`; `callback_needed` requires a non-zero `callback_at` and forbids the
others; `failed` requires non-blank `fail_reason`; `aborted` forbids the extras). `finish_call` is
exclusive and terminal: it is never returned as an ordinary `ports.ToolCall`, and after a valid
outcome the session is finished.

## Errors and retries

Each package exposes a safe `APIError` (status + type/code/param only, never response text) with
`Temporary()` (429 or 5xx) and `RetryDelay()` (parsed `Retry-After`). WebSocket failures surface
as `ProtocolError` with a stable code. **No automatic retries, reconnects, or fallbacks**: C3/C5
owns backoff via `Temporary`/`RetryDelay`. Context cancellation is wrapped so
`errors.Is(err, context.Canceled/DeadlineExceeded)` works. Nothing logs API keys, auth headers,
transcripts, prompts, tool arguments, outcome detail, patient data, or raw audio.

## Live smoke test

Optional, build-tagged, double-gated — excluded from CI and `make verify`:

```bash
export MISTRAL_API_KEY="$(sed -n 's/^MISTRAL_API_KEY=//p' .env)"
LIVE_MISTRAL=1 /Library/Developer/CommandLineTools/usr/bin/make live-test-mistral
```

It runs one short sequence: TTS `Guten Tag.` → canonical frames → realtime STT → final transcript
containing `guten` → one dialogue `capture_info` call (not executed). Result 2026-11-17: **PASS**
(40 canonical frames; float32 PCM encoding confirmed; two close-after-done robustness amendments
recorded in `plans/03-mistral-ai.md` §13).
