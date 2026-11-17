# Insteadly

[![CI](https://github.com/artemidius/Insteadly/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/artemidius/Insteadly/actions/workflows/ci.yml)
[![Release](https://github.com/artemidius/Insteadly/actions/workflows/release.yml/badge.svg)](https://github.com/artemidius/Insteadly/actions/workflows/release.yml)

AI voice assistant that makes phone calls on behalf of users — starting with booking
medical appointments in Vienna, Austria. The backend is a Go service built as a
**hexagon** (ports & adapters): the core owns all business logic and depends only on
interfaces; telephony, speech, dialogue, storage, payments, and notifications are
swappable adapters.

**Status:** C2 (Mistral STT/TTS/dialogue adapters) is ✅. C3 (call runner + agent engine v0)
is **✅ code complete** after its real-Mistral no-phone live gate. C4 (conversation:
streaming VAD/turn-taking, barge-in, playback marks/clear, deterministic IVR, hold detection,
bounded structured-outcome repair) is **✅ code complete** after its real-Mistral no-phone
M1 live gate — see [PROGRESS.md](PROGRESS.md). The binary still boots and serves a working API
with **no credentials and no external network calls** (all providers default to deterministic
stubs). Selecting `INSTEADLY_TELEPHONY_PROVIDER=twilio` activates the real Twilio Voice +
Media Streams adapter; selecting `INSTEADLY_{STT,TTS,DIALOGUE}_PROVIDER=mistral` activates the
real Mistral speech and dialogue adapters (see [docs/mistral.md](docs/mistral.md)).

C4's conversation engine is proven credential-free against loopback fake Mistral servers and
scripted fake telephony, and the full C4 unit/race suite is green, plus a real-Mistral
no-phone loopback (barge-in with clear, deterministic IVR, hold, outcome repair). Twilio live
audio and outbound DTMF remain blocked on the current **trial** account, so DESIGN's real-phone M1 exit
(and the earlier real-phone M0 exit) is still deferred. C4 deliberately does **not** implement
acoustic echo cancellation, speaker diarization, recorded audio/transcripts, a free-form
digits tool, a real human transfer (`escalate_to_human` stays honestly `unavailable`), retries,
or any new env/API surface. See [docs/agent.md](docs/agent.md).

Design of record: [DESIGN.md](DESIGN.md). Chunk plans live in [`plans/`](plans/).

## Quickstart

Requirements: Go ≥ 1.27.4 (the module's `go` directive) and `make` (optional).

```bash
make build                # -> bin/insteadly
./bin/insteadly           # listens on 127.0.0.1:8080, data in ./data

curl -s http://127.0.0.1:8080/healthz | jq .
curl -s -X POST http://127.0.0.1:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{"clinic":{"name":"Praxis Test","phone":"+431234567"},
       "patient":{"full_name":"Max Mustermann","phone":"+436641234567","reason":"test"}}'
```

Zero configuration is required: all providers default to `stub` and storage to
`./data`. The API is described in [docs/api.md](docs/api.md); internals in
[docs/architecture.md](docs/architecture.md), with the call runner/agent engine in
[docs/agent.md](docs/agent.md). There is deliberately **no public place-a-call endpoint** in
C4: a future scheduler invokes `Runner.Run` directly until C5 owns authenticated,
idempotent orchestration.

> macOS note: `/usr/bin/make` is a shim that may fail while `xcode-select` points at a
> broken Xcode. If `make` errors with `xcodebuild ... -find make`, run
> `/Library/Developer/CommandLineTools/usr/bin/make` instead, or `brew install make`.

## Configuration

Configuration comes from environment variables with the `INSTEADLY_` prefix. A
`./.env` file is read when present (one `KEY=VALUE` per line, `#` comments; no quotes
or escapes). Precedence: **process environment > `.env` > defaults**. Unknown
`INSTEADLY_*` names are a startup error (typo protection). Non-prefixed variables are
ignored.

| Variable | Default | Values / notes |
|---|---|---|
| `INSTEADLY_HTTP_ADDR` | `127.0.0.1:8080` | `host:port` |
| `INSTEADLY_HTTP_PUBLIC_URL` | — | public **https** origin Twilio reaches (tunnel URL); required when telephony is twilio in `voice-api` mode |
| `INSTEADLY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `INSTEADLY_LOG_FORMAT` | `text` | `text`, `json` |
| `INSTEADLY_TELEPHONY_PROVIDER` | `stub` | `stub`, `twilio` |
| `INSTEADLY_TWILIO_ACCOUNT_SID` | — | **secret** |
| `INSTEADLY_TWILIO_AUTH_TOKEN` | — | **secret** |
| `INSTEADLY_TWILIO_PHONE_NUMBER` | — | caller ID, E.164 |
| `INSTEADLY_TWILIO_MODE` | `voice-api` | `voice-api`, `sip-trunk` (SIP trunk lands in C7) |
| `INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS` | `0` | Twilio `Timeout` (0 = Twilio default ~60 s); trial accounts reject this parameter |
| `INSTEADLY_STT_PROVIDER` | `stub` | `stub`, `mistral` |
| `INSTEADLY_TTS_PROVIDER` | `stub` | `stub`, `mistral` |
| `INSTEADLY_DIALOGUE_PROVIDER` | `stub` | `stub`, `mistral` |
| `INSTEADLY_STORAGE_PROVIDER` | `local` | `local` |
| `INSTEADLY_STORAGE_LOCAL_DIR` | `./data` | storage root |
| `INSTEADLY_RECORDING_ENABLED` | `false` | global default (DESIGN.md §3.4) |
| `INSTEADLY_TRANSCRIPT_ENABLED` | `false` | global default |
| `INSTEADLY_AUDIO_ENABLED` | `false` | global default |
| `INSTEADLY_MISTRAL_API_KEY` | — | **secret**; required only when a Mistral provider is selected |
| `INSTEADLY_MISTRAL_API_BASE_URL` | `https://api.mistral.ai` | absolute **https** origin |
| `INSTEADLY_MISTRAL_STT_REALTIME_MODEL` | `voxtral-mini-transcribe-realtime-3` | |
| `INSTEADLY_MISTRAL_STT_BATCH_MODEL` | `voxtral-mini-transcribe-3` | |
| `INSTEADLY_MISTRAL_STT_STREAMING_DELAY_MS` | `480` | 80..1200, or exactly 2400 |
| `INSTEADLY_MISTRAL_TTS_MODEL` | `voxtral-mini-tts-3-0` | |
| `INSTEADLY_MISTRAL_TTS_VOICE` | `voice_447` | |
| `INSTEADLY_MISTRAL_TTS_MODE` | `realtime` | `realtime`, `offline` |
| `INSTEADLY_MISTRAL_DIALOGUE_MODEL` | `mistral-small-2603` | |

Secrets are never logged; startup logs and `/healthz` use a sanitized summary. `.env`
is git-ignored and must never be committed. Only `INSTEADLY_*` names are read: the
unprefixed `TWILIO_*` variables in an existing `.env` must be mirrored (or exported) as
`INSTEADLY_TWILIO_*`. Twilio setup, the public tunnel, and the live-call test are described
in [docs/twilio.md](docs/twilio.md).

Configuring a provider whose adapter does not exist yet fails fast with a clear
`not implemented (lands in chunk Cx)` error instead of a half-wired process.

Mistral setup, the exact protocols, the 24→16 kHz audio conversion, the tool/outcome
contract, and the live-test recipe are described in [docs/mistral.md](docs/mistral.md).

## Development

| Command | What it does |
|---|---|
| `make build` | build `bin/insteadly` (version via `VERSION=...`) |
| `make run` | `go run ./cmd/insteadly` |
| `make test` | `go test -race -count=1 ./...` |
| `make cover` | tests with coverage + one-line total |
| `make live-test` | **opt-in** real Twilio call (`-tags live`); needs a public tunnel, credentials and credit |
| `make live-test-mistral` | **opt-in** real Mistral smoke (`-tags live`); needs `LIVE_MISTRAL=1` and `MISTRAL_API_KEY` |
| `make live-test-m0` | **opt-in** real-Mistral, no-phone full-chain M0 loop (`-tags live`); needs `LIVE_MISTRAL=1` and `MISTRAL_API_KEY` |
| `make live-test-m0-phone` | **deferred** real-phone M0 (`-tags live`); needs `LIVE_M0_PHONE=1`, `LIVE_TWILIO=1`, `LIVE_MISTRAL=1`, `TEST_PHONE`, Twilio config, a tunnel and an **upgraded** Twilio account; rings a real phone |
| `make verify` | `lint` + `test` + `check-deps` + `build` (what CI enforces) |
| `make lint` | gofmt check + `go vet` (+ `golangci-lint` when installed) |
| `make check-deps` | fails if `internal/core` imports an adapter (hexagon guard) |
| `make fmt` | `gofmt -w .` |
| `make clean` / `make clean-data` | remove `bin/` / `data/` |

Dependencies: exactly **one** third-party module — `github.com/coder/websocket` v1.8.15
(pure Go, zero transitive dependencies) for the Twilio Media Streams and Mistral realtime
WebSocket clients. A further dependency needs a plan-level decision.

### Layout

```
cmd/insteadly/            process entrypoint (config -> app -> http.Server; drains runner
                          before HTTP shutdown)
internal/core/domain/     pure types + order state machine (stdlib only)
internal/core/ports/      the six+1 port contracts the core consumes
internal/core/agent/      answered-call loop: greeting, M0 gate, STT, tools, TTS
internal/core/runner/     single-attempt call runner: claim, dial, events, Finish, Hangup
internal/core/config/     env loader, validation, secret-safe summary
internal/adapters/        implementations: storage/local, stub, telephony/twilio,
                          stt/mistral, tts/mistral, dialogue/mistral
internal/app/             composition root (the only core+adapters importer)
internal/httpapi/         JSON API, middleware, error envelope
```

Dependency law (enforced by `make check-deps`):

```
cmd → internal/app, internal/httpapi, internal/core/config
internal/app → core/{agent,runner,config,ports,domain} + adapters  (composition root only)
internal/httpapi → core/{ports,domain,config} + app
internal/adapters/* → core/{ports,domain}
internal/core/agent → core/{ports,domain} + stdlib/log/slog
internal/core/runner → core/agent + core/{ports,domain} + stdlib/log/slog
internal/core/ports → internal/core/domain
internal/core/domain → stdlib only
```

## CI/CD

GitHub Actions runs on every push to `main`, every pull request, and on demand
([ci.yml](.github/workflows/ci.yml)):

| Job | Checks |
|---|---|
| **Lint & format** | `gofmt -l`, `go vet`, `make check-deps` (hexagon guard), golangci-lint |
| **Test (race)** | `go test -race -count=1` with coverage; `coverage.out` uploaded as an artifact |
| **Build & smoke test** | `make build`, boots the binary against a temp data dir, checks `/healthz` and a real order create; uploads the binary on `main` |

Releases ([release.yml](.github/workflows/release.yml)) are cut from `v*` tags: the
workflow re-runs the tests, cross-compiles `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`
with the tag as `main.version`, writes `checksums.txt`, smoke-tests the Linux artifact, and
publishes a GitHub release (marked pre-release when the tag contains `-`). Deployment is
deliberately not automated yet — the deployment target is still open (DESIGN.md §6.5), so
the release artifact is the delivery boundary for now.

```bash
git tag v0.1.0
git push origin v0.1.0
```

Action versions are pinned to immutable release tags and kept current by
[Dependabot](.github/dependabot.yml) (weekly, grouped; also watches Go modules).
Lint rules live in [.golangci.yml](.golangci.yml). To reproduce CI locally: `make verify`
(and `make cover`). CI needs only the default `GITHUB_TOKEN` (`contents: read`; `contents:
write` for releases).

## Roadmap

| Chunk | Delivers | Status |
|---|---|---|
| C0 | foundation: hexagon skeleton, config, local storage, stubs, HTTP API | ✅ |
| C1 | Twilio telephony adapter (webhooks, Media Streams, µ-law→PCM16, DTMF) | 🔵 code complete; live audio blocked by Twilio trial |
| C2 | Mistral STT / TTS / dialogue adapters | ✅ code complete; live smoke passed |
| C3 | call runner + agent engine, end-to-end M0 walk | ✅ code complete; real-Mistral no-phone gate passed; real phone blocked by Twilio trial |
| C4 | conversation: VAD/turn-taking, barge-in, IVR, hold, outcome repair | ✅ code complete; real-Mistral no-phone gate passed; real phone blocked by Twilio trial |
| C5+ | orchestration, web app, swapability | ⬜ |

See [PROGRESS.md](PROGRESS.md) for the authoritative status and session log.
