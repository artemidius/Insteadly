# Insteadly — Progress Register

> Purpose: every new session reads this file first to learn where the project stands.
> **Contract:** [DESIGN.md](DESIGN.md) is the source of truth. Chunk plans derive from it;
> if a plan and the design conflict, the design wins — stop and flag it, don't guess.
>
> **Status legend:** ⬜ planned (not started) · 🔵 in progress · ✅ done (exit criteria met) · ⛔ blocked.

---

## Status at a glance (update every session)

- **Phase:** C0 complete; **C1 code complete** — real dialing, status callbacks, TwiML
  fetching, and hangup were verified against Twilio. Live Media Streams audio is blocked by
  the Twilio **trial** plan (Twilio strips `<Stream>`), so C1 is 🔵 until the account is
  upgraded and the live test passes.
- **Current chunk:** C1 — Twilio telephony adapter. Implementation and automated tests are
  done; see [plans/02-twilio-telephony.md](plans/02-twilio-telephony.md) and the
  amendments recorded there.
- **First action of the next session:** with an **upgraded** Twilio account, start a
  tunnel and run `LIVE_TWILIO=1 make live-test`; if it passes (media stream attached, tone
  heard), mark C1 ✅ and then write `plans/03-mistral-ai.md` (C2).
- **Blockers:** live Media Streams audio only — Twilio trial accounts strip `<Stream>`
  from TwiML ([trial TwiML restrictions](https://www.twilio.com/docs/usage/trials/try-out-voice#custom-twiml-during-trial)).
  No code blocker. Operational notes: `golangci-lint` is not installed (optional; a local
  v2.15.0 copy under `/tmp` was used, 0 issues); `/usr/bin/make` is broken by the
  Xcode 16.2/macOS 26 issue — use `/Library/Developer/CommandLineTools/usr/bin/make`
  (verified) or `brew install make`.

---

## Chunk roadmap

| Chunk | Milestone | What it delivers | Plan file | Status |
|---|---|---|---|---|
| **C0** | M0 | Foundation: Go module, hexagon skeleton (domain, all 6 port contracts, config loader, local storage adapter, stub providers, composition root, HTTP API). Zero external deps, zero credentials. | [plans/01-foundation.md](plans/01-foundation.md) | ✅ |
| **C1** | M0 | Twilio telephony adapter (voice-api): webhooks, Media Streams WebSocket, µ-law→PCM16, DTMF. | [plans/02-twilio-telephony.md](plans/02-twilio-telephony.md) | 🔵 code complete; live audio blocked by Twilio trial |
| **C2** | M0 | Mistral adapters for STT / TTS / Dialogue (raw WS/JSON APIs, no Go SDK exists). | `plans/03-mistral-ai.md` (TBD) | ⬜ |
| **C3** | M0 | Call runner + agent engine v0; end-to-end walk: dial owner's mobile, speak through the whole Mistral chain. M0 exit. | `plans/04-m0-e2e.md` (TBD) | ⬜ |
| **C4** | M1 | Conversation: VAD / turn-taking, barge-in, DTMF through IVRs, structured outcome extraction. | `plans/05-conversation.md` (TBD) | ⬜ |
| **C5** | M2 | Orchestration: order store on Postgres, scheduler (windows, retries, callback re-scheduling), status API + SSE. | `plans/06-orchestration.md` (TBD) | ⬜ |
| **C6** | M3 | Web app (React+TS): registration, payments (Stripe), order form, dashboard. | `plans/07-web-app.md` (TBD) | ⬜ |
| **C7** | M4 | Swapability proof + hardening: second telephony adapter (Telnyx or Twilio SIP trunk), one alternative AI provider, recording/storage flags end-to-end, notifications. | `plans/08-swapability.md` (TBD) | ⬜ |

Chunk plans are written **one chunk ahead** in the same style as the C0 plan (scope,
preconditions, exact contracts, steps with verify commands, test plan, definition of done).
C1's plan is ready; before starting C2, write `plans/03-mistral-ai.md`.

---

## Session log (most recent first)

### 2026-11-17 — C1 implementation session (Europe/Vienna)

- Implemented [plans/02-twilio-telephony.md](plans/02-twilio-telephony.md); automated
  checks are green. Committed on `feat/c1-twilio-telephony` (PR opened in this session).
- **New package** `internal/adapters/telephony/twilio`: `adapter.go` (New/Dial/registry/
  Handler), `client.go` (REST create/update/end, APIError), `session.go` (CallSession,
  ordered events, queues, Hangup, reaper, `StreamReady`), `stream.go` (Media Streams
  WebSocket, start validation, µ-law frames), `webhook.go` (voice/status/stream-status
  routes, signature gate), `twiml.go`, `signature.go`, `audio.go`, plus unit + fake-Twilio
  integration tests.
- **Wiring/config:** `App.Webhooks` mounted by `httpapi` only for twilio;
  `INSTEADLY_HTTP_PUBLIC_URL` and `INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS` (default 0 =
  Twilio's ~60 s, opt-in); strict validation; `Summary` keys. One dependency:
  `github.com/coder/websocket v1.8.15`; `go.sum` added. CI cache path → `go.sum`.
- **Automated verification (observed):**
  - `make verify` → gofmt/vet clean, `go test -race` all packages `ok`,
    `OK: core imports no adapters`, binary built.
  - `golangci-lint v2.15.0` (copied to `/tmp`) → **0 issues**.
  - `make cover` → **84.1 %** total statements; twilio package **83.8 %**.
  - `go test -tags live -run TestLiveCall ./internal/app` without `LIVE_TWILIO` → skipped.
- **Boot matrix (observed):** stub boots and serves orders (healthz `telephony=stub`);
  twilio + public URL boots (healthz `telephony=twilio`, unsigned status webhook 403);
  missing public URL and `sip-trunk` fail with the expected `config:`/`not implemented`
  errors.
- **Live retry (01:02, user available):** the call rang and was answered, then completed;
  the live test failed at "Media Stream never attached" as designed. Twilio's trial
  substitution of `<Stream>` is the only reason no audio plays — a real call path is
  working.
- **Live Twilio verification (real calls, trial account):** dialing → ringing → answered →
  status callbacks → TwiML fetch → completed all worked; the TwiML fetch carries **no**
  `X-Twilio-Signature` (status callbacks do), so the voice route accepts an unsigned fetch
  only for a known call ID + matching `AccountSid`/`CallSid`. No Media Stream ever attached
  and Twilio emitted no stream-status callback: a **trial account strips `<Stream>`**
  (documented in the trial TwiML reference). The live test now fails explicitly when
  `StreamReady()` never fires. **Live audio is blocked on an account upgrade**, not on
  code.
- **Trial Calls API finding:** trial accounts reject optional Calls parameters
  (`Invalid or disallowed parameters ... trial accounts have limited parameter access`).
  The adapter now sends only `To`, `From`, `Url`, `StatusCallback`, and repeated
  `StatusCallbackEvent`; `Method`/`StatusCallbackMethod`/`Record` are omitted and
  `Timeout` is opt-in.
- **Deviations from the plan** (all recorded in the plan's Amendments):
  ring-timeout default 0 instead of 30; unsigned TwiML fetch guard; `statusCallback`
  diagnostic route accepts unsigned requests; µ-law 0x7f canonicalization in the test;
  `LIVE_TWILIO`/`LIVE_DTMF` instead of `INSTEADLY_LIVE_*` (strict config rejects them);
  `StreamReady()` test hook.
- **Docs:** README status/config/dependency/roadmap, `docs/architecture.md`,
  `docs/api.md`, new `docs/twilio.md`, Makefile `live-test`.
- **Environment:** cloudflared **2026.11.0** was downloaded to `/tmp` (not installed);
  its quick tunnel, including a WSS probe, worked. Twilio account is **Trial**; the shared
  trial number works for outbound calls, but `<Stream>` is blocked.

### 2026-11-17 — C1 planning session (Europe/Vienna)

- Wrote [plans/02-twilio-telephony.md](plans/02-twilio-telephony.md): full C1 plan
  (scope, preconditions, Twilio→port mapping, exact routes/events/audio pipeline,
  implementation steps, test matrix, exit criteria, risks). No C1 code written.
- Decisions recorded in the plan (rationale there):
  - **Dependency:** `github.com/coder/websocket` **v1.8.15** (pure Go, zero transitive
    deps, MIT) — Go has no stdlib WebSocket server. Pin it and switch CI caching to
    `go.sum`.
  - **No legacy-env fallback:** C1 keeps the C0 rule that only `INSTEADLY_*` variables are
    read. The unprefixed Twilio names in `.env` must be mirrored/exported by the user
    (`TEST_PHONE` stays test-only). This keeps `TestNonInsteadlyEnvIgnored` valid.
  - **Outbound DTMF:** Twilio Media Streams has no outbound `dtmf` message; plan uses the
    call-update/TwiML `<Play digits>` + stream-reconnect path, with an explicit live-test
    decision gate (fallback: `ErrDTMFUnsupported`, move to C4). Inbound DTMF is direct.
  - **Config:** add `INSTEADLY_HTTP_PUBLIC_URL` (public https origin for webhooks) and
    `INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS` (default 30). Webhook routes:
    `/webhooks/telephony/twilio/{voice,status,stream,stream-status}/{callID}`.
  - **Live verification** needs a public HTTPS/WSS tunnel and costs trial credit, so it
    requires explicit user approval; if blocked, C1 must not be marked ✅.
- Pre-plan verification (observed): `go test -count=1 ./...` → all packages `ok`; Go
  **1.27.4 darwin/arm64**; `cloudflared`, `ngrok`, and the Twilio CLI are **absent**;
  `go.sum` absent; `.env` has `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`,
  `TWILIO_PHONE_NUMBER`, `TEST_PHONE` (names only, values never read); remote
  `origin git@github.com:artemidius/Insteadly.git`, branch `main`.
- Twilio docs re-checked 2026-11-17 (Media Streams message set, `<Connect>/<Stream>`,
  `<Play digits>`, Call resource/status callbacks, webhook signatures); sources are listed
  in the plan. No design conflict found.
- Nothing committed; `.env`, `bin/`, `data/` untouched.

### 2026-11-17 — CI/CD session (Europe/Vienna)

- Added GitHub Actions CI/CD; no application behaviour changed except one lint-driven
  error-wrapping fix.
  - `.github/workflows/ci.yml`: on push to `main`, PRs, manual — **lint** (gofmt, go vet,
    hexagon guard, golangci-lint), **test** (`-race`, coverage artifact), **build** +
    live smoke test (healthz + order create); binary artifact on `main`.
  - `.github/workflows/release.yml`: on `v*` tags — tests, cross-compile
    `linux/{amd64,arm64}` + `darwin/{amd64,arm64}`, SHA-256 checksums, artifact smoke
    test (version match), `gh release create` (auto pre-release for hyphenated tags).
  - `.github/dependabot.yml`: weekly grouped updates for GitHub Actions and Go modules.
  - `.golangci.yml`: golangci-lint v2 config (standard set + errorlint, misspell, nilerr,
    nolintlint, unconvert, wastedassign; gofmt/goimports formatters).
  - `Makefile`: `verify` (lint+test+check-deps+build) and `cover`; `.gitignore`:
    `coverage.out`, `dist/`; README: CI/CD section + badges.
- Fix: local storage now wraps malformed-ID errors with `%w: %w` (errorlint), so the
  message reads `order <id>: not found: invalid order id ...`.
- **Verification (local):** actionlint 1.7.12 → clean; golangci-lint 2.15.0 → **0
  issues**; gofmt clean; `go vet` clean; `make check-deps` → OK; `go test -race` all
  packages green, **83.8 %** statement coverage; `make build VERSION=citest`; CI smoke
  script green (healthz 200, order 201, 32-hex id); release cross-compile produced four
  static binaries + checksums (`file(1)` verified ELF/Mach-O targets).
- Pinned action versions (checked via GitHub API on 2026-11-17): `actions/checkout`
  v7.0.1, `actions/setup-go` v7.0.0, `actions/upload-artifact` v7.0.2,
  `golangci/golangci-lint-action` v9.3.0, `golangci-lint` v2.15.0.
- Deployment is intentionally **not** automated: DESIGN.md §6.5 (deployment target) is
  still open, so release artifacts are the CD boundary for now.
- Not committed/pushed in this session (no request yet).

### 2026-11-16 — C0 implementation session (Europe/Vienna)

- Implemented [plans/01-foundation.md](plans/01-foundation.md) end to end; all §7 exit
  criteria met. No commits made (`.env`, `data/`, `bin/` stay untracked/ignored).
- **Toolchain:** `brew install go` → **go1.27.4 darwin/arm64** (`/opt/homebrew/bin/go`).
  Module `github.com/artemidius/insteadly`, `go 1.27.4`, **zero external requires**.
- **Files created:** `go.mod`, `.gitignore`, `Makefile`, `README.md`,
  `docs/{architecture,api}.md`, `cmd/insteadly/main.go`,
  `internal/core/{domain,ports,config}/…`, `internal/adapters/storage/local/…`,
  `internal/adapters/stub/…`, `internal/app/{app,providers,logger}.go`,
  `internal/httpapi/{server,orders}.go`, plus `*_test.go` in every package with logic.
- **Verification (commands + observed results):**
  - `go vet ./...` → clean. `go test -race -count=1 ./...` → all packages `ok`
    (domain, config, local storage, stub, app, httpapi).
  - `/Library/Developer/CommandLineTools/usr/bin/make lint` → gofmt clean, vet clean,
    `golangci-lint not installed — skipped`.
  - `make check-deps` → `OK: core imports no adapters`.
  - `make build` → `bin/insteadly` (built with `-trimpath`, `CGO_ENABLED=0`).
  - Live smoke with **no `INSTEADLY_*` env**: `/healthz` → 200 `status:ok`, providers
    stub/local; `POST /api/v1/orders` → 201 + `Location` + 32-hex id, status `created`;
    list length 1; `GET /api/v1/orders/<id>` → `created`; `data/orders/<id>.json` exists
    with mode `0600`; unknown id → 404; `DELETE /api/v1/orders` → 405; unknown JSON field
    / oversized body covered by tests; SIGTERM → `shutting down`, exit code 0.
  - Negative startup: `INSTEADLY_TELEPHONY_PROVIDER=twilio` → exit 1
    `not implemented (lands in chunk C1)`; `INSTEADLY_STORAGE_PROVIDER=bogus` → exit 1
    validation; `INSTEADLY_NOPE=1` → exit 1 `unknown variable`;
    `INSTEADLY_RECORDING_ENABLED=yes` → exit 1 bool parse error.
- **Deviations from the plan (deliberate, small):**
  1. `internal/app` is split into `app.go` / `providers.go` / `logger.go`, and
     `internal/httpapi` into `server.go` / `orders.go`, to keep files smaller (user
     request). Package boundaries and dependency law are exactly as planned.
  2. Local-storage read paths map a malformed order ID to `ports.ErrNotFound` (such an
     ID can never exist), so the API answers 404 without duplicating the ID regex.
  3. Added `README.md`, `docs/architecture.md`, `docs/api.md` (user asked for docs; the
     plan's tree did not list them).
- **Environment notes:** `golangci-lint` still absent (optional);
  `/usr/bin/make` is broken by the Xcode 16.2/macOS 26 incompatibility
  (`xcodebuild ... -find make` → exit 34304) — use the CommandLineTools `make` or
  `brew install make`. The smoke-test `data/` directory was removed afterwards.
- **Next:** write `plans/02-twilio-telephony.md`, then start C1.

### 2026-11-16 — planning session (Europe/Vienna)

- DESIGN.md finalized (2026-11-16): Go hexagon backend, Twilio + Mistral, milestones M0–M4.
  Status per design: design only, no code written.
- Created [plans/01-foundation.md](plans/01-foundation.md): detailed C0 implementation plan
  (module path, exact directory tree, verbatim Go contracts for domain + all six ports,
  config surface with `INSTEADLY_` env prefix, local storage adapter spec, stub providers,
  composition root, HTTP API, full test matrix, exit criteria with smoke commands).
- Created this PROGRESS.md (register + update protocol).
- Environment verified (names only, values never printed):
  - **Go is NOT installed** on this machine (`go: command not found` everywhere checked) →
    `brew install go` is C0 step 0. Record the installed version here when done.
  - `golangci-lint` not installed (optional; `make lint` works without it).
  - `.env` contains: `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `TWILIO_PHONE_NUMBER`,
    `TEST_PHONE`. **Missing:** `MISTRAL_API_KEY` — required before C2's real Mistral calls.
  - No git commits, no git remote. Node 26 present (irrelevant until C6).
- Nothing implemented yet. Next: C0 per the plan.

---

## Environment facts every session needs

- **Workspace:** `/Users/q/Insteadly` — project name **Insteadly** (i-n-s-t-e-a-d-l-y).
- **Module path:** `github.com/artemidius/insteadly` — one word after the final slash:
  `artemidius/` + `insteadly`. A doubled `in` (`ininsteadly`) is the wrong spelling and must
  never be typed.
- **Go toolchain:** go1.27.4 darwin/arm64 at `/opt/homebrew/bin/go`; module directive
  `go 1.27.4`. C1 adds exactly one module, **`github.com/coder/websocket` v1.8.15** (pure
  Go, zero transitive deps); `go.sum` has two lines. Keep the dependency list this short;
  any further dependency is a plan-level decision.
- **Twilio account:** currently **Trial**. Status callbacks/TwiML fetch work; the trial
  strips `<Stream>` (no live audio until upgrade) and rejects optional Calls parameters
  (`Method`, `Timeout`, `Record`, `StatusCallbackMethod`, ...). The TwiML `Url` fetch is
  **unsigned**; status callbacks are signed. Live-test flags are `LIVE_TWILIO=1` and
  `LIVE_DTMF=1` (unprefixed so strict config ignores them).
- **make:** use `/Library/Developer/CommandLineTools/usr/bin/make` (the `/usr/bin/make`
  shim is broken by the Xcode 16.2/macOS 26 issue); `golangci-lint` optional/absent
  (CI installs v2.15.0 itself).
- **CI/CD:** GitHub Actions — `ci.yml` (push/PR/manual: lint, race tests, build+smoke),
  `release.yml` (`v*` tags: cross-compiled binaries + checksums + GitHub release),
  Dependabot weekly. Local parity: `make verify` / `make cover`; CI-only linter:
  `golangci-lint` per `.golangci.yml`.
- **Binary / command dir:** `cmd/ininsteadly` is wrong — correct is `cmd/insteadly`, binary
  `bin/insteadly`.
- **Config:** env prefix `INSTEADLY_`, dots→underscores (e.g. `INSTEADLY_TELEPHONY_PROVIDER`);
  precedence real env > `.env` > defaults; unknown `INSTEADLY_*` vars are a startup error
  (strict mode). Secrets are never logged and never printed.
- **Canonical audio:** PCM16 LE mono 16 kHz everywhere inside the core (`domain.FrameBytes`
  = 640 B = 20 ms). All codec conversion happens inside adapters.
- **Credentials:** `.env` must never be committed (it is git-ignored). The app reads it only
  through the registry of `INSTEADLY_*` names; the **unprefixed** Twilio names already in
  `.env` are not read (C1 decision — mirror/export them as `INSTEADLY_TWILIO_*`). `TEST_PHONE`
  is read only by the build-tagged live test. `MISTRAL_API_KEY` still missing (needed C2).
- **Tunnel:** `cloudflared` 2026.11.0 works from `/tmp` (quick tunnel; HTTP + WSS verified);
  it is **not installed** globally. The tunnel URL must equal `INSTEADLY_HTTP_PUBLIC_URL`
  (scheme + host, no trailing slash) or Twilio webhook signatures fail with 403.
- **C1 webhook surface:** `/webhooks/telephony/twilio/{voice,status,stream,stream-status}/{callID}`,
  mounted only when `telephony.provider=twilio`. Status callbacks require a valid
  `X-Twilio-Signature`; Twilio does **not** sign the TwiML `Url` fetch (verified live), so
  the voice route requires a known call ID plus matching `AccountSid`/`CallSid`. The WSS
  handshake is validated by AccountSID + call ID + media format. Trial accounts strip
  `<Stream>`; live audio needs an upgraded account.

---

## Update protocol (obligatory for every session)

1. **Session start:** read DESIGN.md → this file → the current chunk's plan (see roadmap
   table). Check the roadmap and session log for where work stopped.
2. **During work:** follow the current plan exactly. Deviations are allowed only with a
   reason; log them in the session entry — do not silently edit a plan that is being
   implemented. Plans become read-only once implementation of that chunk starts; amend via
   a dated "Amendments" subsection inside the plan file if unavoidable.
3. **Session end (always):**
   - append a session log entry (date, timezone; what was done; files changed; exact
     commands run + observed results; what remains);
   - update "Status at a glance";
   - update the chunk table row: 🔵 when starting a chunk, ✅ when its exit criteria are met
     (with the evidence), ⛔ with the concrete reason if blocked;
   - record new environment facts (installed tools/versions, new `.env` key *names only*).
4. **Chunk done:** mark ✅, then write the next chunk's plan file before writing any code
   for it (same structure as `plans/01-foundation.md`).
5. **Never:** commit unless the user asks; force-push or rewrite history; print or copy
   secrets; read `.env` values into logs.

---

## Conventions (stable decisions, see also C0 plan §5)

- Errors wrap with `%w`; sentinels live in `ports`/`domain`. Logs via `log/slog`, snake_case
  keys, `request_id` on every HTTP log line. JSON field names snake_case, timestamps RFC3339.
- The core (`internal/core`) never imports adapters — enforced by `make check-deps`.
- Only `internal/app` (composition root) imports both core and adapters.
- `make` targets: `build`, `run`, `test` (with `-race`), `lint`, `check-deps`, `fmt`,
  `clean`, `clean-data`. `golangci-lint` optional.

---

## Open design questions (DESIGN.md §6) — where they get resolved

| # | Question | Resolve in |
|---|---|---|
| 1 | Dialogue tool surface + outcome JSON schema | C2/C3 (adapters + agent engine) |
| 2 | Scheduler semantics (retries/backoff; reception-hours source) | C5 |
| 3 | Clinic directory (praxisplan.at, licensing, freshness) | C5/C6 |
| 4 | Payments provider confirmation (Stripe assumed) | before C6 |
| 5 | Deployment target (EU region preferred) | before C7 |
| 6 | Compliance gate (EU AI Act, GDPR, TKG, abuse prevention) | before production, per design |
| 7 | Coarse `DialogueEngine` port — adopt now or later | C3 decision point |

