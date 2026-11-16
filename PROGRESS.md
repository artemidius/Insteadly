# Insteadly — Progress Register

> Purpose: every new session reads this file first to learn where the project stands.
> **Contract:** [DESIGN.md](DESIGN.md) is the source of truth. Chunk plans derive from it;
> if a plan and the design conflict, the design wins — stop and flag it, don't guess.
>
> **Status legend:** ⬜ planned (not started) · 🔵 in progress · ✅ done (exit criteria met) · ⛔ blocked.

---

## Status at a glance (update every session)

- **Phase:** C0 complete — the binary boots, serves the API, and is fully tested with
  zero credentials and zero external network calls.
- **Current chunk:** C1 — Twilio telephony adapter. C0 plan
  [plans/01-foundation.md](plans/01-foundation.md) is implemented (✅).
- **First action of the next session:** write `plans/02-twilio-telephony.md` (one chunk
  ahead, per the update protocol), then implement C1.
- **Blockers:** none for C0. Operational notes: `golangci-lint` is not installed
  (optional); `/usr/bin/make` is broken by the Xcode 16.2/macOS 26 issue — use
  `/Library/Developer/CommandLineTools/usr/bin/make` (verified) or `brew install make`.

---

## Chunk roadmap

| Chunk | Milestone | What it delivers | Plan file | Status |
|---|---|---|---|---|
| **C0** | M0 | Foundation: Go module, hexagon skeleton (domain, all 6 port contracts, config loader, local storage adapter, stub providers, composition root, HTTP API). Zero external deps, zero credentials. | [plans/01-foundation.md](plans/01-foundation.md) | ✅ |
| **C1** | M0 | Twilio telephony adapter (voice-api): webhooks, Media Streams WebSocket, µ-law→PCM16, DTMF. | `plans/02-twilio-telephony.md` (write before starting) | ⬜ |
| **C2** | M0 | Mistral adapters for STT / TTS / Dialogue (raw WS/JSON APIs, no Go SDK exists). | `plans/03-mistral-ai.md` (TBD) | ⬜ |
| **C3** | M0 | Call runner + agent engine v0; end-to-end walk: dial owner's mobile, speak through the whole Mistral chain. M0 exit. | `plans/04-m0-e2e.md` (TBD) | ⬜ |
| **C4** | M1 | Conversation: VAD / turn-taking, barge-in, DTMF through IVRs, structured outcome extraction. | `plans/05-conversation.md` (TBD) | ⬜ |
| **C5** | M2 | Orchestration: order store on Postgres, scheduler (windows, retries, callback re-scheduling), status API + SSE. | `plans/06-orchestration.md` (TBD) | ⬜ |
| **C6** | M3 | Web app (React+TS): registration, payments (Stripe), order form, dashboard. | `plans/07-web-app.md` (TBD) | ⬜ |
| **C7** | M4 | Swapability proof + hardening: second telephony adapter (Telnyx or Twilio SIP trunk), one alternative AI provider, recording/storage flags end-to-end, notifications. | `plans/08-swapability.md` (TBD) | ⬜ |

Chunk plans are written **one chunk ahead**: before starting C1, write `plans/02-*.md` in the
same style as the C0 plan (scope, preconditions, exact contracts, steps with verify commands,
test plan, definition of done).

---

## Session log (most recent first)

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
  `go 1.27.4`. Keep `go.mod` bare (zero external requires).
- **make:** use `/Library/Developer/CommandLineTools/usr/bin/make` (the `/usr/bin/make`
  shim is broken by the Xcode 16.2/macOS 26 issue); `golangci-lint` optional/absent.
- **Binary / command dir:** `cmd/ininsteadly` is wrong — correct is `cmd/insteadly`, binary
  `bin/insteadly`.
- **Config:** env prefix `INSTEADLY_`, dots→underscores (e.g. `INSTEADLY_TELEPHONY_PROVIDER`);
  precedence real env > `.env` > defaults; unknown `INSTEADLY_*` vars are a startup error
  (strict mode). Secrets are never logged and never printed.
- **Canonical audio:** PCM16 LE mono 16 kHz everywhere inside the core (`domain.FrameBytes`
  = 640 B = 20 ms). All codec conversion happens inside adapters.
- **Credentials:** `.env` must never be committed (it is git-ignored). C0 does not read it.
  `MISTRAL_API_KEY` still missing. A tunnel (cloudflared/ngrok) will be needed for real-call
  testing in C1/C3.

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

