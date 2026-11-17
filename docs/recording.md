# Recording and transcripts (C7)

C7 makes the recording flags that have existed since C0 **effective**. The flags were
parsed and surfaced in `config.Summary` but nothing persisted them; recording persistence
was deliberately deferred (`plans/05-conversation.md` §11, `plans/06-orchestration.md` §1).
With every flag off (the default) behavior is byte-for-byte unchanged: the runner wraps the
dialed session in a no-op and writes zero bytes.

The implementation lives in `internal/core/recorder` (core: it imports only `domain`,
`ports`, and stdlib). It is wired **always** in `internal/app` so a per-order override can
turn capture on even when the global default is off; with capture off, `Wrap` returns the
original session and starts no goroutine.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `INSTEADLY_RECORDING_ENABLED` | `false` | **master capture gate** |
| `INSTEADLY_AUDIO_ENABLED` | `false` | persist call audio as WAV (requires the master gate) |
| `INSTEADLY_TRANSCRIPT_ENABLED` | `false` | persist a post-call transcript (requires the master gate) |

Per-order overrides are `domain.Order.Recording` (`*RecordingFlags`): each `*bool` field is
nil = fall through to the global value. Resolution (`recorder.Resolve`) is:

1. `recording` (per-order if set, else global) is the master gate. `false` ⇒ capture
   nothing, regardless of `audio`/`transcript`.
2. `capture = audio || transcript` (you must capture audio in memory to transcribe).
3. `wav = audio`, `transcript = transcript`.

| global | per-order | effective |
|---|---|---|
| rec=false | nil | capture nothing |
| rec=true, audio=true, tscript=false | nil | WAV only |
| rec=true, audio=false, tscript=true | nil | transcript only (capture in memory, discard audio) |
| rec=true, audio=true, tscript=true | nil | WAV + transcript |
| rec=false | `{Recording:&true, Audio:&true}` | WAV (per-order overrides global) |
| rec=true, audio=false | `{Audio:&true}` | WAV (field-level override) |
| rec=true, audio=true | `{Audio:&false}` | transcript only (if transcript=true), else nothing |

> `transcript.enabled` with `audio.enabled=false` still captures audio **in memory** — it
> must, to transcribe. It simply discards the WAV at the storage boundary. This is not a
> flag-enforcement bug.

## Capture points

The recorder tees the `ports.CallSession` boundary with a wrapper (`recorder.Wrap`), the one
runner seam added in C7:

- **Inbound (clinic) audio** — a relay goroutine copies every canonical chunk from the
  underlying `Inbound()` channel into an inbound builder and forwards it unchanged.
- **Outbound (assistant) audio** — `SendAudio` appends every non-empty canonical chunk to
  the outbound builder, then forwards it.
- **Transcript** — at finalize, `ports.SpeechToText.TranscribeFile` runs over the captured
  **raw inbound PCM** with `BatchOptions{Language:"de"}` (the batch adapter wraps raw PCM in
  a WAV multipart itself).

Neither the agent's listener nor the speaker changes; the tee lives entirely in the session
wrapper. The wrapper forwards every `ports.CallSession` method, including the optional
`StreamReady() <-chan struct{}` diagnostic, so the Twilio readiness wait still works when
recording is on.

## Artifacts

Two mono WAVs (one per direction) and one text transcript are written through the existing
`ports.BlobStore` (the local adapter in C5/C6; there is still no Postgres/S3 blob store):

| Artifact | `BlobKey.Kind` | `BlobKey.Name` |
|---|---|---|
| inbound recording | `recording` | `<callID>-inbound.wav` |
| outbound recording | `recording` | `<callID>-outbound.wav` |
| transcript | `transcript` | `<callID>-transcript.txt` |

`BlobKey.OrderID` is the order id. `<callID>` is the session's `domain.CallID` (32
lowercase hex for real adapters), so a retry with a new call id never overwrites an earlier
attempt's artifacts. Names are plain filenames validated by the local store's traversal
guard.

WAV format: RIFF/WAVE, **PCM16 LE, mono, 16 kHz**, a 44-byte header plus raw PCM (stdlib
`encoding/binary`; same header as `stt/mistral/wav.go`, re-implemented in core because the
core must not import an adapter).

Transcript text:

```
insteadly transcript
order=<orderID>
call=<callID>
clinic=<clinic name>
outcome=<outcome kind>
---
<TranscribeFile().Text>
```

`outcome=` is empty when the order had no outcome at capture time. The recorder's contract
(`New(stt, blobs, policy, log)`) has no `OrderStore`, and the runner persists the terminal
outcome *after* the call; the header therefore documents the outcome known at wrap time
(normally none). This is a deliberate, documented limitation, not a bug.

## Finalize semantics

- Finalize is triggered by the inbound relay observing the adapter's channel close (the
  normal path) and, as a fallback, by `Hangup` (a caller that stops draining `Inbound`).
- It is `sync.Once`-guarded and idempotent, runs off the calling goroutine, and uses a
  fresh 30 s `context.WithTimeout` (it must not inherit a canceled call context).
- It is **best-effort**: an STT or blob error is logged and never fails the call or the
  runner's terminal persist. If the transcript fails, the WAVs are still persisted.
- Capture is capped per direction at 64 MiB (≈35 minutes of canonical PCM) as a memory
  safety net; reaching the cap is logged once and stops further capture for that direction.
- A nil blob store or STT is tolerated (logged/skipped), which keeps hand-built recorders in
  tests safe.

## Verification

- `internal/core/recorder`: the full override matrix, WAV header/round-trip, tee
  forwarding/verbatim capture, idempotent finalize, nil-capture returning the original
  session, `StreamReady` forwarding, STT/blob failure tolerance.
- `internal/core/runner`: the wrap is applied exactly once, with the claimed order; nil
  recorder and flags-off leave the session identical.
- `internal/app/recording_matrix_test.go`: the real runner + recorder + local storage for
  every §7.4 row, asserting the exact blob set (and none when off).
- `internal/app/swapability_test.go`: recording on through the real Telnyx/OpenAI adapters,
  asserting the WAVs, transcript contents, and raw PCM handed to `TranscribeFile`.
