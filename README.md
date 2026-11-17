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
M1 live gate. C5 (M2 orchestration: Postgres order store, scheduler with retries and
callback requeue, status API + SSE, place-call eligibility endpoint) is **✅ code complete**.
C6 (M3 web app: React+TS registration/order form/dashboard, cookie auth, Stripe Checkout +
signed webhook, stub payments, SPA embedded in the Go binary) is **🔵 code complete pending
the conductor's final verification** — see [PROGRESS.md](PROGRESS.md). The binary still
boots and serves a working API with **no credentials and no external network calls** (all
providers default to deterministic stubs; `auth.mode=off` and `payments.provider=stub` are
the defaults). Selecting `INSTEADLY_TELEPHONY_PROVIDER=twilio` activates the real Twilio
Voice + Media Streams adapter; selecting `INSTEADLY_{STT,TTS,DIALOGUE}_PROVIDER=mistral`
activates the real Mistral speech and dialogue adapters (see
[docs/mistral.md](docs/mistral.md)).

C4's conversation engine is proven credential-free against loopback fake Mistral servers and
scripted fake telephony, and the full C4 unit/race suite is green, plus a real-Mistral
no-phone loopback (barge-in with clear, deterministic IVR, hold, outcome repair). C5 adds
the durable orchestration layer: a Postgres `OrderStore` + lease-claiming `SchedulerStore`
(`FOR UPDATE SKIP LOCKED`), an in-process scheduler with opening windows (Europe/Vienna),
bounded concurrency, retry backoff, history-preserving callback requeue, an order-status SSE
stream, and the API-key-gated place-call eligibility endpoint.

C6 adds the M3 web milestone: a React 19 + TypeScript + Vite SPA (register/login, order
form, dashboard with live SSE), served by the same Go binary with the production bundle
embedded behind the `webembed` build tag (the default Go build still needs no Node). User
auth uses opaque 256-bit session tokens, `sha256`-at-rest, `HttpOnly`/`SameSite=Lax`
cookies, and bcrypt password hashes; `auth.mode=cookie` scopes order routes to the owner and
answers `404` for another user's order. Payments are Stripe Checkout Sessions with a
signature-verified `checkout.session.completed` webhook, or a deterministic `stub` provider
(the default) whose authenticated simulate endpoint drives the same `Order.MarkPaid` seam.
`POST /orders/{id}/call` and `INSTEADLY_API_KEY` remain the unchanged C5 dev/ops path;
`Runner.Run` remains the only dial path. Twilio live audio and outbound DTMF remain blocked
on the current **trial** account, so DESIGN's real-phone M1 exit (and the earlier real-phone
M0 exit) is still deferred. C6 deliberately does **not** implement River/a job queue,
per-clinic opening hours (Q3), notifications, refunds/subscriptions, recording persistence,
or production hardening (CSRF token, password reset, email verification, rate limiting).
See [docs/architecture.md](docs/architecture.md) and [PROGRESS.md](PROGRESS.md).

Design of record: [DESIGN.md](DESIGN.md). Chunk plans live in [`plans/`](plans/).

## Quickstart

Requirements: Go ≥ 1.27.4 (the module's `go` directive) and `make` (optional). Node ≥ 22.12
+ npm are needed only for `make build` (SPA embed), `make web`, and frontend development;
the Go-only `make verify` does not need Node.

```bash
make build                # builds the SPA and embeds it -> bin/insteadly (needs Node/npm)
./bin/insteadly           # listens on 127.0.0.1:8080, data in ./data
# open http://127.0.0.1:8080/ for the web app (auth defaults to off; see below)

curl -s http://127.0.0.1:8080/healthz | jq .
curl -s -X POST http://127.0.0.1:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{"clinic":{"name":"Praxis Test","phone":"+431234567"},
       "patient":{"full_name":"Max Mustermann","phone":"+436641234567","reason":"test"}}'
```

Zero configuration is required: all providers default to `stub`, storage to `./data`, and
the scheduler is enabled. In the default `auth.mode=off` the web UI's auth calls answer
`501`; set `INSTEADLY_AUTH_MODE=cookie` to use registration/login and owner-scoped orders
(the default stub payments simulate checkout without a Stripe account):

```bash
INSTEADLY_AUTH_MODE=cookie ./bin/insteadly
# http://127.0.0.1:8080/ -> Create account -> Place order -> Pay and schedule
```

For frontend development with hot reload, run the Go API and Vite separately; Vite proxies
`/api`, `/webhooks`, and `/healthz` to `127.0.0.1:8080`:

```bash
INSTEADLY_AUTH_MODE=cookie ./bin/insteadly       # terminal 1
cd web && npm ci && npm run dev                  # terminal 2 -> http://127.0.0.1:5173/
```

Backend-only builds and CI do **not** need Node: `make verify` uses `make build-go` and the
`web` package compiles its placeholder handler. `make web` runs `npm ci`, the Vite build,
and Vitest.

To let the scheduler dial an order in the no-env/demo flow, make it eligible through the
place-call endpoint (the scheduler picks it up outside opening windows only when the window
allows; the default is Mon–Fri 08:00–18:00 Europe/Vienna):

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/orders/<id>/call
curl -N http://127.0.0.1:8080/api/v1/orders/<id>/events   # live status SSE
```

The API is described in [docs/api.md](docs/api.md); internals in
[docs/architecture.md](docs/architecture.md), with the call runner/agent engine in
[docs/agent.md](docs/agent.md). `POST /call` is an **eligibility** endpoint, never a
synchronous dialer: `Runner.Run` remains the only dial path.

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
| `INSTEADLY_STORAGE_PROVIDER` | `local` | `local`, `postgres` |
| `INSTEADLY_STORAGE_LOCAL_DIR` | `./data` | storage root; also hosts blobs in postgres mode |
| `INSTEADLY_POSTGRES_DSN` | — | **secret**; full libpq/URL DSN, required when provider is `postgres` |
| `INSTEADLY_SCHEDULER_ENABLED` | `true` | run the in-process scheduler |
| `INSTEADLY_SCHEDULER_TICK` | `5s` | eligibility scan interval |
| `INSTEADLY_SCHEDULER_MAX_ATTEMPTS` | `3` | total dial attempts including the first |
| `INSTEADLY_SCHEDULER_MAX_CONCURRENT` | `8` | in-flight `Runner.Run` calls |
| `INSTEADLY_SCHEDULER_LEASE` | `2m` | claim lease (eligibility guard; the call timeout is 6m) |
| `INSTEADLY_SCHEDULER_BUSY_BACKOFF` | `15m` | base retry delay for `busy` |
| `INSTEADLY_SCHEDULER_NO_ANSWER_BACKOFF` | `30m` | base retry delay for `no_answer`/`ivr_dead_end` |
| `INSTEADLY_SCHEDULER_ERROR_BACKOFF` | `5m` | base retry delay for `error` |
| `INSTEADLY_SCHEDULER_WINDOWS` | Mon–Fri 08:00–18:00 Europe/Vienna | JSON opening windows `[{"dow":"mon","start":"08:00","end":"18:00"},…]` |
| `INSTEADLY_API_KEY` | — | **secret**; Bearer key for `POST /call`; empty disables auth (dev only) |
| `INSTEADLY_API_IDEMPOTENCY_TTL` | `10m` | in-process idempotency-key lifetime |
| `INSTEADLY_AUTH_MODE` | `off` | `off` (C5 unauthenticated API) or `cookie` (registration/login + owner-scoped orders) |
| `INSTEADLY_AUTH_SESSION_TTL` | `24h` | session lifetime; also the cookie `Max-Age` |
| `INSTEADLY_AUTH_COOKIE_NAME` | `insteadly_session` | 1–64 letters/digits/`_`/`-` |
| `INSTEADLY_AUTH_BCRYPT_COST` | `12` | bcrypt cost, 4–31 |
| `INSTEADLY_PAYMENTS_PROVIDER` | `stub` | `stub` (simulate checkout) or `stripe` |
| `INSTEADLY_STRIPE_SECRET_KEY` | — | **secret**; required when provider is `stripe` |
| `INSTEADLY_STRIPE_WEBHOOK_SECRET` | — | **secret**; required when provider is `stripe`; `whsec_…` |
| `INSTEADLY_STRIPE_PRICE_ID` | — | one Stripe Price id for the prototype Checkout Session |
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

Stripe mode additionally requires `INSTEADLY_HTTP_PUBLIC_URL` (Checkout success/cancel URLs
are absolute) and is the only configuration that mounts the signed webhook route. The stub
payments provider and `auth.mode=off` remain the zero-configuration defaults.

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
| `make build` | build the SPA and `bin/insteadly` with the bundle embedded (`webembed`); needs Node |
| `make build-go` | backend-only build (placeholder SPA); no Node, used by `make verify` |
| `make web` | `cd web && npm ci && npm run build && npm test` (Vite build + Vitest) |
| `make web-build` | `cd web && npm ci && npm run build` (no tests) |
| `make run` | `go run ./cmd/insteadly` |
| `make test` | `go test -race -count=1 ./...` |
| `make test-postgres` | `go test -tags postgres -count=1 ./...`; **requires Docker** (testcontainers `postgres:16-alpine`), not part of `verify` |
| `make cover` | tests with coverage + one-line total |
| `make live-test` | **opt-in** real Twilio call (`-tags live`); needs a public tunnel, credentials and credit |
| `make live-test-mistral` | **opt-in** real Mistral smoke (`-tags live`); needs `LIVE_MISTRAL=1` and `MISTRAL_API_KEY` |
| `make live-test-m0` | **opt-in** real-Mistral, no-phone full-chain M0 loop (`-tags live`); needs `LIVE_MISTRAL=1` and `MISTRAL_API_KEY` |
| `make live-test-m0-phone` | **deferred** real-phone M0 (`-tags live`); needs `LIVE_M0_PHONE=1`, `LIVE_TWILIO=1`, `LIVE_MISTRAL=1`, `TEST_PHONE`, Twilio config, a tunnel and an **upgraded** Twilio account; rings a real phone |
| `make verify` | `lint` + `test` + `check-deps` + `build-go` (Go-only, no Node/Docker/credentials) |
| `make lint` | gofmt check + `go vet` (+ `golangci-lint` when installed) |
| `make check-deps` | fails if `internal/core` imports an adapter (hexagon guard) |
| `make fmt` | `gofmt -w .` |
| `make clean` / `make clean-data` | remove `bin/` / `data/` |

Dependencies (Go): `github.com/coder/websocket` v1.8.15 (Twilio Media Streams and Mistral
realtime WebSockets), `github.com/jackc/pgx/v5` v5.13.0 (Postgres driver/pool, C5), and
`github.com/stripe/stripe-go/v87` v87.1.0 (Checkout + signed webhooks, C6). C6 also promotes
`golang.org/x/crypto` v0.58.0 to a direct dependency for bcrypt.
`github.com/testcontainers/testcontainers-go` v0.45.0 and its Postgres module are
**test-only** and build-tagged `postgres`. No ORM and no second SQL driver. Frontend
dependencies are pinned in `web/package-lock.json` (React 19, Vite 8, TypeScript 7, Vitest
5); there is no router, state library, or ORM. A further dependency needs a plan-level
decision.

### Layout

```
cmd/insteadly/            process entrypoint (config -> app -> http.Server; drains runner
                          before HTTP shutdown)
internal/core/domain/     pure types + order state machine (stdlib only)
internal/core/ports/      the six+1 port contracts the core consumes
internal/core/agent/      answered-call loop: greeting, M0 gate, STT, tools, TTS
internal/core/runner/     single-attempt call runner: claim, dial, events, Finish, Hangup
internal/core/scheduler/  eligibility loop, windows, retries, callback requeue
internal/core/events/     in-process order-status hub (runner/scheduler → SSE)
internal/core/auth/       register/login/logout, opaque sessions, sha256-at-rest
internal/core/config/     env loader, validation, secret-safe summary
internal/adapters/        implementations: storage/local, storage/postgres, stub,
                          telephony/twilio, stt/mistral, tts/mistral, dialogue/mistral,
                          password/bcrypt, payments/{stub,stripe}
internal/app/             composition root (the only core+adapters importer)
internal/httpapi/         JSON API, auth/checkout/webhook routes, middleware, error envelope
web/                      React+TS+Vite SPA; embedded only under the webembed build tag
```

Dependency law (enforced by `make check-deps`):

```
cmd → internal/app, internal/httpapi, internal/core/config
internal/app → core/{agent,runner,config,ports,domain} + adapters  (composition root only)
internal/httpapi → core/{ports,domain,config} + app
internal/adapters/* → core/{ports,domain}
internal/core/agent → core/{ports,domain} + stdlib/log/slog
internal/core/runner → core/agent + core/{events,ports,domain} + stdlib/log/slog
internal/core/scheduler → core/{events,ports,domain} + stdlib/log/slog
internal/core/events → internal/core/domain + stdlib
internal/core/auth → core/{ports,domain} + stdlib only (no bcrypt)
internal/core/config → stdlib only (no pgx/stripe-go/bcrypt; adapters parse their settings)
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
| **Build & smoke test** | `make build` (sets up Node, builds the SPA with `webembed`), boots the binary against a temp data dir, checks `/healthz` and a real order create; uploads the binary on `main` |

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
| C5 | orchestration: Postgres order store, scheduler, status SSE, place-call | ✅ code complete; make verify + test-postgres green |
| C6 | web app (React+TS registration/order form/dashboard), Stripe payments, cookie auth | 🔵 code complete pending conductor verification; Twilio/live-phone + Q5 deployment blockers remain |
| C7 | swapability, recording, notifications, production hardening | ⬜ |

See [PROGRESS.md](PROGRESS.md) for the authoritative status and session log.
