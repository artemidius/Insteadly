# Insteadly

AI voice assistant that makes phone calls on behalf of users — starting with booking
medical appointments in Vienna, Austria. The backend is a Go service built as a
**hexagon** (ports & adapters): the core owns all business logic and depends only on
interfaces; telephony, speech, dialogue, storage, payments, and notifications are
swappable adapters.

**Status:** C0 (foundation) implemented — see [PROGRESS.md](PROGRESS.md). The binary
boots and serves a working API with **no credentials and no external network calls**:
telephony, STT, TTS, and dialogue are deterministic stubs; storage is local disk.

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
[docs/architecture.md](docs/architecture.md).

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
| `INSTEADLY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `INSTEADLY_LOG_FORMAT` | `text` | `text`, `json` |
| `INSTEADLY_TELEPHONY_PROVIDER` | `stub` | `stub`, `twilio` (adapter lands in C1) |
| `INSTEADLY_TWILIO_ACCOUNT_SID` | — | **secret** |
| `INSTEADLY_TWILIO_AUTH_TOKEN` | — | **secret** |
| `INSTEADLY_TWILIO_PHONE_NUMBER` | — | caller ID (C1) |
| `INSTEADLY_TWILIO_MODE` | `voice-api` | `voice-api`, `sip-trunk` (C1) |
| `INSTEADLY_STT_PROVIDER` | `stub` | `stub`, `mistral` (C2) |
| `INSTEADLY_TTS_PROVIDER` | `stub` | `stub`, `mistral` (C2) |
| `INSTEADLY_DIALOGUE_PROVIDER` | `stub` | `stub`, `mistral` (C2) |
| `INSTEADLY_STORAGE_PROVIDER` | `local` | `local` |
| `INSTEADLY_STORAGE_LOCAL_DIR` | `./data` | storage root |
| `INSTEADLY_RECORDING_ENABLED` | `false` | global default (DESIGN.md §3.4) |
| `INSTEADLY_TRANSCRIPT_ENABLED` | `false` | global default |
| `INSTEADLY_AUDIO_ENABLED` | `false` | global default |
| `INSTEADLY_MISTRAL_API_KEY` | — | **secret** (C2) |

Secrets are never logged; startup logs and `/healthz` use a sanitized summary. `.env`
is git-ignored and must never be committed.

Configuring a provider whose adapter does not exist yet fails fast with a clear
`not implemented (lands in chunk Cx)` error instead of a half-wired process.

## Development

| Command | What it does |
|---|---|
| `make build` | build `bin/insteadly` (version via `VERSION=...`) |
| `make run` | `go run ./cmd/insteadly` |
| `make test` | `go test -race -count=1 ./...` |
| `make lint` | gofmt check + `go vet` (+ `golangci-lint` when installed) |
| `make check-deps` | fails if `internal/core` imports an adapter (hexagon guard) |
| `make fmt` | `gofmt -w .` |
| `make clean` / `make clean-data` | remove `bin/` / `data/` |

Dependencies: standard library only (`go.mod` has no `require` block). Please keep it
that way in the foundation; a new dependency needs a plan-level decision.

### Layout

```
cmd/insteadly/            process entrypoint (config -> app -> http.Server)
internal/core/domain/     pure types + order state machine (stdlib only)
internal/core/ports/      the six+1 port contracts the core consumes
internal/core/config/     env loader, validation, secret-safe summary
internal/adapters/        implementations: storage/local, stub providers
internal/app/             composition root (the only core+adapters importer)
internal/httpapi/         JSON API, middleware, error envelope
```

Dependency law (enforced by `make check-deps`):

```
cmd → internal/app, internal/httpapi, internal/core/config
internal/app → core/{config,ports,domain} + adapters        (composition root only)
internal/httpapi → core/{ports,domain,config} + app
internal/adapters/* → core/{ports,domain}
internal/core/ports → internal/core/domain
internal/core/domain → stdlib only
```

## Roadmap

| Chunk | Delivers | Status |
|---|---|---|
| C0 | foundation: hexagon skeleton, config, local storage, stubs, HTTP API | ✅ |
| C1 | Twilio telephony adapter (webhooks, Media Streams, µ-law→PCM16) | ⬜ |
| C2 | Mistral STT / TTS / dialogue adapters | ⬜ |
| C3 | call runner + agent engine, end-to-end M0 walk | ⬜ |
| C4+ | conversation, orchestration, web app, swapability | ⬜ |

See [PROGRESS.md](PROGRESS.md) for the authoritative status and session log.
