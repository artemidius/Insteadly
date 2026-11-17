# Insteadly — Progress Register

> Purpose: every new session reads this file first to learn where the project stands.
> **Contract:** [DESIGN.md](DESIGN.md) is the source of truth. Chunk plans derive from it;
> if a plan and the design conflict, the design wins — stop and flag it, don't guess.
>
> **Status legend:** ⬜ planned (not started) · 🔵 in progress · ✅ done (exit criteria met) · ⛔ blocked.

---

## Status at a glance (update every session)

- **Phase:** C0 ✅; **C1 code complete** (live audio + outbound DTMF blocked by Twilio
  trial); **C2 ✅** (Mistral adapters, fake suites green, live smoke passed); **C3 ✅ code
  complete** (call runner + agent engine v0; real-Mistral no-phone live gate passed);
  **C4 ✅ code complete** (streaming VAD/turn-taking, barge-in with Twilio `clear`/`mark`,
  deterministic IVR, hold detection, bounded structured-outcome repair; real-Mistral
  no-phone M1 live gate passed); **C5 🔵 code complete pending the conductor's final
  verification** — Postgres order/scheduler store (migrations, `FOR UPDATE`, `SKIP
  LOCKED`), in-process scheduler (windows, bounded concurrency, retry backoff,
  history-preserving callback requeue), order-status SSE hub + endpoint, and the
  API-key-gated place-call eligibility endpoint. DESIGN's **real-phone M0 and M1 exits**
  remain blocked by the Twilio trial stripping `<Stream>` (and, for M1, by the unproven
  outbound-DTMF path plus the need for controlled IVR/hold destinations).
- **Current chunk:** C5 (implementation done, PR open, awaiting conductor verification).
  Next: conductor verifies C5, then C6 plan (web app, payments, real auth).
- **First action of the next session:** verify PR #12 / C5 acceptance in `plans/06-orchestration.md`
  §10, then write `plans/07-web-app.md`.
- **Blockers:** DESIGN's real-phone M0/M1 exits remain blocked by the Twilio trial stripping
  `<Stream>` and the unproven outbound-DTMF path — need an upgraded account, a controlled
  IVR/hold destination, and explicit approval before the end-to-end phone walk. C5
  deployment blockers (not code-complete blockers): no real-phone/Mistral evidence was
  produced or re-run, the API key is a prototype gate (C6 owns real auth), the scheduler is
  single-process with no River yet, and multi-instance runner draining is untested.
  Operational: `golangci-lint` absent locally (optional; CI installs v2.15.0; the /tmp
  v2.15.0 binary was run); `/usr/bin/make` broken by the Xcode 16.2/macOS 26 issue — use
  `/Library/Developer/CommandLineTools/usr/bin/make`. Build/test commands may need a
  `/tmp` GOCACHE/GOMODCACHE/GOPATH in this sandbox (`~/Library/Caches/go-build`,
  `~/go/pkg/mod`, and `~/go/pkg/sumdb` are not writable here). Postgres-tagged tests need
  `CGO_ENABLED=0` while Xcode clang is broken.

---

## Chunk roadmap

| Chunk | Milestone | What it delivers | Plan file | Status |
|---|---|---|---|---|
| **C0** | M0 | Foundation: Go module, hexagon skeleton (domain, all 6 port contracts, config loader, local storage adapter, stub providers, composition root, HTTP API). Zero external deps, zero credentials. | [plans/01-foundation.md](plans/01-foundation.md) | ✅ |
| **C1** | M0 | Twilio telephony adapter (voice-api): webhooks, Media Streams WebSocket, µ-law→PCM16, DTMF. | [plans/02-twilio-telephony.md](plans/02-twilio-telephony.md) | 🔵 code complete; live audio blocked by Twilio trial |
| **C2** | M0 | Mistral adapters for STT / TTS / Dialogue (raw WS/JSON APIs, no Go SDK exists). | [plans/03-mistral-ai.md](plans/03-mistral-ai.md) | ✅ code complete; live smoke passed |
| **C3** | M0 | Call runner + agent engine v0; end-to-end walk: dial owner's mobile, speak through the whole Mistral chain. M0 exit. | [plans/04-m0-e2e.md](plans/04-m0-e2e.md) | ✅ code complete; real-Mistral no-phone gate passed; real phone blocked by Twilio trial |
| **C4** | M1 | Conversation: streaming VAD/turn-taking, barge-in, playback clear/marks, deterministic IVR, hold detection, bounded structured-outcome repair. | [plans/05-conversation.md](plans/05-conversation.md) | ✅ code complete; real-Mistral no-phone gate passed; real phone blocked by Twilio trial |
| **C5** | M2 | Orchestration: order store on Postgres, scheduler (windows, retries, callback re-scheduling), status API + SSE, place-call. | [plans/06-orchestration.md](plans/06-orchestration.md) | 🔵 code complete pending conductor verification |
| **C6** | M3 | Web app (React+TS): registration, payments (Stripe), order form, dashboard. | `plans/07-web-app.md` (TBD) | ⬜ |
| **C7** | M4 | Swapability proof + hardening: second telephony adapter (Telnyx or Twilio SIP trunk), one alternative AI provider, recording/storage flags end-to-end, notifications. | `plans/08-swapability.md` (TBD) | ⬜ |

Chunk plans are written **one chunk ahead** in the same style as the C0 plan (scope,
preconditions, exact contracts, steps with verify commands, test plan, definition of done).
C4's plan (`plans/05-conversation.md`) is approved and implemented; its no-phone live gate is
the only remaining C4 exit evidence. The real-phone M0/M1 exits still wait on the Twilio
account upgrade (and, for M1, controlled IVR/hold destinations).

---

## Session log (most recent first)

### 2026-11-17 — C5 implementation session (Europe/Vienna)

- Implemented [plans/06-orchestration.md](plans/06-orchestration.md) end to end on
  `feat/c5-orchestration` in the §7 order 1–13, **without running any live phone/Mistral
  test** and without reading any secret/`.env` value. PR #12 against `main`; not merged.
  Branch/PR details are reported to the conductor; §14 of the plan records the dated
  implementation amendments (Hub struct vs interface, exhausted-outcome rewrite,
  Claim/wall-clock due re-check + fake-clock test alignment, pool close in
  `App.CloseStorage`, single-Update place-call, SSE overrun-on-close, attempt-history
  length after busy→booked, test tag split, internal `buildStorage` signature).
- **Domain/ports (additive only):** `Order.NextAttemptAt` + `AttemptHistory`,
  `AttemptRecord`, `Order.Schedule` (paid→scheduled), `Order.Requeue` (finished →
  scheduled with history append, guardrails for callback/retryable-failed); unchanged
  `Transition`/`Finish`/spine table. New `ports.SchedulerStore`
  (`ListEligible`/`Claim`/`Release`) + `ports.ErrNotEligible`; a claim is a lease, never a
  status change.
- **New core packages:** `internal/core/events` (bounded in-process hub; slow subscriber
  dropped/closed and counted; nil-safe publisher; `Close`/unsubscribe idempotent) and
  `internal/core/scheduler` (injectable `Clock`/`FakeClock`; config windows with DST-safe
  Europe/Vienna math; tick loop + deterministic `RunOnce`/`Wait`; atomic lease claims;
  paid→scheduled via `Schedule`; bounded `Runner.Run` concurrency; outcome→retry table;
  linear backoff advanced to the next window; `callback_needed` requeue at `callback_at`;
  cap → terminal `failed/attempts_exhausted`; release on every exit; `Close` cancels and
  waits in-flight attempts). The scheduler never mutates an order to `dialing`.
- **Runner:** optional `Events events.Publisher`; publishes dialing/ringing/in_call/finished
  after the corresponding persisted transitions; nil publisher is a no-op. The exactly-once
  claim, single `Dial`, one `Finish`, one `Hangup`, and duplicate-dial fence are unchanged
  (existing `TestRunAtomicClaimRejectsConcurrentDuplicate` still proves one dial).
- **Adapters:** local store implements `SchedulerStore` with an in-memory lease map
  (+json-persisted `next_attempt_at`/`attempt_history`); new
  `internal/adapters/storage/postgres` (pgx v5 only) with embedded
  `0001_orders.sql`, advisory-lock + `schema_migrations` migration runner, JSONB
  `Order` mapping, `Create/Get/List/Update` (`SELECT … FOR UPDATE` transaction), and
  `ListEligible`/`Claim` (`FOR UPDATE SKIP LOCKED`)/`Release`. Blobs stay on the local
  adapter via a composite `ports.Storage`.
- **Config:** `storage.provider=postgres` + `INSTEADLY_POSTGRES_DSN` (secret),
  scheduler params (`enabled/tick/max_attempts/max_concurrent/lease/backoffs/windows`),
  `INSTEADLY_API_KEY` (secret) + `INSTEADLY_API_IDEMPOTENCY_TTL`; new duration setter;
  strict windows JSON validation; redacted `postgres.dsn` in `Summary`; `config` remains
  stdlib-only (no pgx). No-env boot unchanged (`storage: local`, scheduler enabled).
- **HTTP:** `POST /api/v1/orders/{id}/call` is an **eligibility** endpoint
  (`created→paid→scheduled(now)`, `202`, `Location`, constant-time Bearer auth when a key
  is configured, in-process `(order,key)` idempotency with TTL, 409 on repeats); no
  synchronous dial path. `GET /api/v1/orders/{id}/events` streams a full-order snapshot
  then live `order` events, `retry: 3000`, 15 s `: ping`, flush per event, overrun ends the
  stream with `event: error`/`overrun`. SSE subscribes only; it never publishes.
- **Composition/lifecycle:** `App.Events`/`App.Scheduler`, `App.Start(ctx)` (idempotent,
  no-op when disabled), `App.Close(ctx)` = scheduler → runner → hub, and
  `App.CloseStorage()` for the Postgres pool last. `cmd/insteadly` starts the scheduler
  before serving and shuts down scheduler → runner → hub → HTTP → pool.
- **Tests added:** domain Schedule/Requeue guardrails + history ordering; events hub
  delivery/drop/close; local scheduler store; runner publisher event sequence
  (success/failure/shutdown abort); scheduler fake-clock retry table, cap, callback,
  windows/DST/quiet, concurrency cap, lease release, Close; config defaults/env/validation/
  redaction; HTTP auth/202/409/idempotent replay/TTL/SSE snapshot+live+heartbeat+overrun+
  disconnect; Postgres-gated (`//go:build postgres`) adapter round-trip/JSONB/row-lock/
  migration/claim-contention/SKIP-LOCKED/lease tests and app-level Postgres composition +
  scheduler retry; untagged `m2_integration_test.go` (local + fake telephony/agent) driving
  create → real place-call → scheduler → busy retry → booked, plus callback requeue and
  SSE status sequence.
- **Observed verification (this sandbox, `/tmp` caches; `CGO_ENABLED=0` for tagged tests):**
  - `/Library/Developer/CommandLineTools/usr/bin/make verify` → **PASS** (gofmt clean,
    `go vet`, `go test -race -count=1 ./...` all green, `check-deps` OK: core imports no
    adapters, `CGO_ENABLED=0` build OK). The target reports golangci-lint as a PATH skip;
    `/tmp/golangci-lint-2.15.0-darwin-arm64/golangci-lint run --timeout=5m` → **0 issues**.
  - `make cover` → total **83.3 %** statements (events 94.0 %, scheduler 84.6 %,
    httpapi 84.4 %, runner 94.3 %, app 78.4 %, config 96.7 %, domain 96.3 %, local 83.1 %;
    the Postgres adapter has no default-tag tests by design). Tagged coverage:
    postgres adapter **73.6 %**, app **82.8 %**.
  - `go test -race -count=10 ./internal/core/scheduler ./internal/core/events
    ./internal/core/runner ./internal/httpapi ./internal/app` → **PASS** (scheduler 1.7 s,
    events 1.8 s, runner 4.8 s, httpapi 12.0 s, app 18.3 s). One earlier identical stress
    run hit a transient 10 s loopback WebSocket read timeout in the pre-existing M0 fake
    Mistral harness; the two subsequent full runs (app alone and the combined command)
    passed, and no C5 test was involved.
  - `go test -tags postgres -count=1 ./internal/adapters/storage/postgres/ ./internal/app/`
    → **PASS** with Docker Engine 29.7.2 (testcontainers `postgres:16-alpine`): adapter
    3.2 s, app 4.1 s. This includes concurrent claim contention (exactly one winner),
    `SKIP LOCKED` locked-row exclusion, migration idempotence, JSONB fidelity, and the
    Postgres-backed scheduler busy→booked retry.
  - `go mod tidy` + `git diff --exit-code -- go.mod go.sum` → **clean**; `go.mod` pins
    `pgx/v5 v5.13.0`, `testcontainers-go v0.45.0` and its Postgres module (test-only,
    `//go:build postgres`); no other direct dependency. `go vet -tags live ./...` clean.
  - `git diff main -- internal/core/domain/order.go internal/core/ports/storage.go` →
    **additive only** (new fields/type/methods + new port; no changed existing behavior).
    `git diff --check main...HEAD` clean. No live phone/Mistral test was run; no secret was
    read or logged (`postgres.dsn` and `api.key` stay out of `Summary`).
- **Status:** C5 is 🔵 code complete pending the conductor's final verification/merge.
  DESIGN's real-phone exits, per-clinic opening hours (Q3), River, payments, recording
  persistence, web UI, and real user auth remain out of scope/blocked as before.

### 2026-11-17 — C4 conductor live-gate session (Europe/Vienna)

- Ran the multi-agent flow for C4: Architect (`kimi-code/k3.1-pro`) → Coder
  (`deepseek-official/deepseek-flash`) → Reviewer (`kimi-code/k3.1-pro`). Merged PR #6
  (`feat/c4-conversation`) and follow-ups PR #7 (live pacing), PR #8 (live script), PR #9
  (greeting barge), PR #10 (VAD barge tuning), PR #11 (outbound accounting) via squash.
- **Real-Mistral no-phone M1 live gate PASS (observed):** `LIVE_MISTRAL=1 make live-test-m1`
  → `TestLiveM1NoPhone` **PASS** (11.7 s): `outcome=aborted`, `attempts=1`, `clears=2`,
  `outbound_frames=79` — proves streaming VAD, a real barge-in with `clear`/history
  reconciliation during the greeting, a real STT final, and a terminal `finish_call` outcome.
  Satisfies plan §8's C4 code-complete gate.
- **Live-gate debugging (conductor-instrumented, multiple iterations):** the gate exposed a
  live-harness burst-feed bug (instant frame delivery overflowed the 64-frame utterance
  queue → paced to 20 ms/frame), a script bug (first utterance finished the call instead of
  eliciting a reply), a greeting-barge restructure, and — measured via real TTS frame RMS —
  a genuine VAD tuning bug: the barge onset required 5 consecutive frames each passing
  `flux >= 0.04`, which real speech never produces (spectral flux only spikes at transitions,
  and real speech has ~3–4-frame loud runs with consonant dips). Fixed by dropping the
  per-frame flux requirement for the barge path and `BargeInFrames` 5→3, keeping the higher
  0.024/4×-noise threshold for echo asymmetry. All recorded in `plans/05-conversation.md`
  §3.7/§13.
- **Status:** C4 → ✅ code complete. DESIGN's real-phone M1 exit remains blocked by the
  Twilio trial + unproven outbound DTMF + controlled IVR/hold destinations; the
  `live-test-m1-phone` harness compiles/skips and is ready for the upgraded-account run.
- **Environment:** no new env keys/deps; the real `MISTRAL_API_KEY` was read only for the
  no-phone live gate (value never printed). Builds used a `/tmp` GOCACHE/GOMODCACHE.

### 2026-11-17 — C4 implementation session (Europe/Vienna)

- Implemented [plans/05-conversation.md](plans/05-conversation.md) end to end on
  `feat/c4-conversation`, **without running any live test** (per explicit instruction) and
  without reading any secret/`.env` value. PR #6 against `main`; not merged.
- **Conversation engine (`internal/core/agent/`):** new `audio_features.go` (stdlib PCM16
  decode, DC-removed RMS/ZCR, Hann+Goertzel 39-bin grid + 350/440/480/620 probes, spectral
  flux/flatness, strong-tone/peak tests), `vad.go` (adaptive noise floor, 60 ms onset,
  300 ms bounded pre-roll, 700 ms hangover, 15 s forced boundary, barge thresholds),
  `listen.go` (continuous pump; one STT worker per utterance; bounded live feed; one
  closing/pending final; malformed/overflow failures), `speak.go` (bounded text segments;
  one TTS generation per segment; mark wait; ack timeout), `bargein.go` (bounded 2 s
  `Abort`+`ClearAudio`, join ordering, confirmed-only rollback), `ivr.go` (German/English
  menu parser, unique target only, one digit/menu, loop/ambiguity/step/timeout dead ends),
  `hold.go` (3 s tone/cadence/music slate), `recovery.go` (fixed controls, two per call,
  45 s stall, tool-consistency/empty/invalid repair), plus `control.go` (mark/DTMF router),
  prompt greeting-disclosure variants, and a full agent/state-machine rewrite. The C3
  `gate.go`/`transcribe.go` and their tests were replaced.
- **Ports/domain/adapters:** added `EventPlaybackMark`+`PlaybackID`, required
  `CallSession.ClearAudio`/`MarkPlayback`, `DialogueSession.Interrupt`/`SendControl`,
  `SpeechStream.Abort`; updated stubs, all fakes, and live-tagged files. Twilio now sends
  ordered `clear`/`mark` with a playback epoch, queue purge, residual-encoder reset, and
  inbound-mark normalization. Mistral TTS realtime/offline gained non-flushing `Abort`;
  Mistral dialogue gained history rollback/reopen and system-role control turns.
- **Runner:** separate capacity-64 post-answer DTMF/mark relay with overflow-as-error; the
  terminal channel stays capacity-one/independent. Pre-answer playback marks are contract
  errors. Agent outcomes (incl. direct `ivr_dead_end`) still win; one `Finish`/`Hangup`.
- **Tests:** new DSP/VAD/listen/speaker/barge/IVR/hold/recovery/control suites, Twilio
  clear/mark/epoch/concurrency tests, Mistral interrupt/control/abort tests, stub lifecycle
  tests, runner relay tests, a C3-compatible booked M0 chain adapted to streaming, and
  `internal/app/m1_integration_test.go` covering streaming turns, barge confirmed history,
  IVR digit→booked and ambiguity dead end, hold/no-STT/resume/timeout, invalid-outcome
  repair, booked-without-confirmation repair, and recovery exhaustion. Live harnesses
  `TestLiveM1NoPhone`/`TestLiveM1Phone` compile and SKIP without gates.
- **Observed verification (this sandbox, `/tmp` GOCACHE/GOMODCACHE):**
  - `/Library/Developer/CommandLineTools/usr/bin/make verify` → **PASS** (gofmt, `go vet`,
    `go test -race -count=1 ./...` all green, `check-deps` OK: core imports no adapters,
    `CGO_ENABLED=0 go build ./cmd/insteadly` OK). `make verify` reports golangci-lint as a
    PATH skip; the `/tmp` v2.15.0 binary run separately reports **0 issues**.
  - `make cover` → total **87.3 %** statements: agent **89.4 %**, runner **93.0 %**, Twilio
    **83.9 %**, dialogue/mistral **89.2 %**, tts/mistral **87.9 %**, app **81.4 %**, stub
    **87.7 %**, stt/mistral **84.5 %**, config 96.5 %, domain 93.8 %, httpapi 88.7 %,
    storage 81.7 %.
  - `go test -race -count=10 ./internal/core/agent ./internal/core/runner
    ./internal/adapters/telephony/twilio ./internal/adapters/dialogue/mistral
    ./internal/adapters/tts/mistral ./internal/app` → **PASS** (agent 104.6 s; others
    3.0–15.7 s).
  - `go test -tags live -run 'TestLiveM1(NoPhone|Phone)$' ./internal/app` → both **SKIP**
    (no `LIVE_MISTRAL`/`LIVE_M1_PHONE` gates set). `go vet -tags live ./...` → clean.
  - `go list -m all` → only the main module + `github.com/coder/websocket v1.8.15`;
    `go mod tidy && git diff --exit-code -- go.mod go.sum` → unchanged.
  - `git diff main -- internal/core/config internal/httpapi docs/api.md` → empty. No new
    `INSTEADLY_*`/`Summary()` keys, no public call endpoint, no new module dependency.
  - GitHub Actions PR #6 CI (run 44147234745): **Lint & format pass**, **Test (race) pass**,
    **Build & smoke test pass**. A first CI attempt exposed a platform-specific exact-frame
    assumption in the M0 integration streaming assertion; commit `99d6200` now asserts the
    actual contract (one contiguous ordered pre-roll+live STT window through the hangover).
- **Deviations/decisions** are recorded in `plans/05-conversation.md` §13 (no replay pacer;
  wall-clock IVR wait; hold classification in assistant mode; Twilio ordered item queue;
  recovery always rolls back before control; music-hold synthetic fixture; unchanged
  config/API/module surfaces). No live test was run and no secret was read.
- **Post-review corrections (PR #6 review):** fixed all five blockers and both contract
  gaps — (1) STT end-of-input now closes the feed channel so every already-accepted frame
  is drained before `Close`; (2) IVR purpose/imperative evidence is option-local
  (hard-punctuation clauses + nearest-digit partitions; `terminvergabe` added); (3) human
  speech and confirmed hold clear the post-digit IVR wait; (4) the runner fails on
  control-relay overflow even if the agent raced to a valid outcome; (5) a segment is
  confirmed only after at least one nonempty canonical frame was sent (`errNoPlaybackAudio`
  otherwise); (6) listener/dialogue cleanup gets a fresh bounded budget at Run exit; (7)
  hold cadence requires the same dominant probe, two similar on-bursts, and an internal off
  gap. New regression tests:
  `TestUtteranceWorkerDrainsQueuedFramesBeforeClose`,
  `TestUtteranceWorkerSlowFeedStillDrainsAcceptedFrames`,
  `TestListenerDelayedHandshakeDrainsEveryAcceptedFrame`,
  `TestIvrOfficeHoursMenuDoesNotPressAppointmentDigit`,
  `TestIvrOptionLocalPurposeSelectsOnlyTheMatchingDigit`,
  `TestIvrHumanActivityCancelsPostDigitTimeout`,
  `TestRunnerControlOverflowNeverPersistsAValidOutcome`,
  `TestPlayerRequiresNonemptyPlaybackAudio`,
  `TestRunSilentSynthesisDoesNotUnlockTools`,
  `TestRunCleanupBudgetIsFreshAtExit`,
  `TestHoldDetectorCadenceRequiresSimilarSameProbeBursts`. The previously observed M0
  integration drop was stress-run `-race -count=100` (PASS) and listener feed/end races
  `-race -count=100` (PASS). Dated details are in `plans/05-conversation.md` §13.
- **Post-fix verification (observed):** `make verify` PASS; `make cover` total **87.5 %**
  (agent 89.6 %, runner 94.1 %, Twilio 83.7 %, dialogue 89.2 %, tts 87.9 %, app 81.4 %);
  required `-race -count=10` PASS (agent 112.2 s, app 15.0 s, others 2.4–5.8 s); live M1
  tests SKIP/SKIP; `go vet -tags live ./...` clean; golangci-lint v2.15.0 **0 issues**;
  `go.mod`/`go.sum`, config/httpapi/api drift, and `git diff --check` unchanged/clean.
- **Post-merge live-gate pacing fix (harness only):** the conductor's first
  `TestLiveM1NoPhone` run on merged `main` (`750dffe`) failed with
  `agent: utterance queue overflow`. The live harness had burst-written the whole ~600-frame
  synthesized caller utterance; the 64-frame STT worker queue cannot drain during a real
  0.6–0.9 s WebSocket handshake. Real telephony feeds 20 ms/frame. `m1_live_test.go` now uses
  `feedPacedWhileRunning` (one frame per 20 ms, stops on `runDone`) for both scripted
  utterances; `sendFrames` remains the instant burst for credential-free integration tests.
  No production VAD/queue/`audioFeedBuffer` change. Dated details in
  `plans/05-conversation.md` §13.
- **Post-merge live-gate script fix (test content only):** the re-run on `92d8cdf` failed
  because the first scripted utterance instructed the model to book AND abort, so the real
  model returned `finish_call(aborted)` with no assistant text; there was no assistant audio
  to barge (`clears=0`, greeting-only outbound). `first` is now exactly
  `"Guten Tag. Ich möchte bitte einen Termin vereinbaren."`; the abort instruction stays only
  in the `second` barge utterance. `awaitAudioGrowth` now needs only
  `liveReplyGrowthFrames = 5` new frames (100 ms), so a short-but-real reply is not rejected.
  No production change. Dated details in `plans/05-conversation.md` §13.
- **CI deflake (test only):** PR #8's first CI run exposed the post-review
  `TestIvrHumanActivityCancelsPostDigitTimeout` using a 60 ms menu window; under CI
  scheduling the human final could arrive after the timeout. The window is now 1 s with a
  1.2 s post-turn wait (the invariant is stale-timer clearing, not tight timing); the test
  passed `-race -count=100` locally. No production change.
- **Post-merge live-gate barge-target fix (test content only):** the `b114c4f` gate run
  reached the barge step but observed `clears=0`. Both STT finals were correct, but the real
  assistant reply was short (~2.9 s) and quiet, so the acoustic barge onset did not confirm
  before it finished (STT also dropped the initial "Stopp"). `TestLiveM1NoPhone` now barges
  during the fixed ~8.8 s greeting: it waits only for the first audible greeting frame (not
  the two marks), then feeds one paced
  `"Stopp, Stopp! Bitte beenden Sie den Anruf jetzt mit dem Ergebnis abgebrochen, …"`
  utterance. The test asserts aborted/attempts=1/clears>=1/non-silent audio/one hangup and
  drops the model-reply `awaitMarks`/`awaitAudioGrowth` gating. Production barge is
  unchanged; the synthetic M1 scenario still covers it. Dated details in
  `plans/05-conversation.md` §13.
- **Barge-onset VAD tuning (production change, conductor-authorized):** the `35d3512` gate
  still saw `clears=0`; measured real-TTS frames showed why. Real speech has only 3–4 loud
  consecutive frames before consonant/phoneme dips (e.g. RMS 0.0046, 0.0373, 0.0946, 0.0897,
  0.0366, 0.0035, …), and spectral flux spikes only at transitions, so the old
  “5 consecutive flux-qualified frames” barge rule was unreachable in practice. Barge onset
  now requires **3 consecutive frames / 60 ms** above `max(normal, 4·noise, 0.024)` with
  flatness/tone checks and **no per-frame flux**; normal listening onset still requires flux.
  `BargeInFrames` default 5→3; the stronger energy floor retains asymmetry (still no AEC).
  The live cue now starts with measured speech energy: `"Guten Tag. Bitte beenden Sie den
  Anruf jetzt mit dem Ergebnis abgebrochen, …"`. Added
  `TestVADBargeFiresOnSteadyLoudVoicedFramesWithoutFlux` and
  `TestVADBargeFiresOnMeasuredSpeechProfile`; updated synthetic barge/listener thresholds.
  Dated §3.7 + §13 notes. No live test run.
- **Live outbound-audio accounting (test harness only):** the `d56ec91` gate run confirmed
  the tuned barge works (`clears >= 1`, terminal `aborted`) but failed on `"no real outbound
  audio"`: `integrationSession.ClearAudio` correctly clears the queued `sentAudio` slice, and
  the live test sampled it after the barge clear. `SendAudio` now also maintains cumulative
  `sentAudioTotal`/`sentAudioNonZero`/`sentAudioBadLen` that `ClearAudio` never resets; the
  live test asserts those, and a credential-free
  `TestIntegrationSessionOutboundAccountingSurvivesClear` regression covers the semantics. No
  production change. Dated details in `plans/05-conversation.md` §13.
- **Status:** C4 → 🔵 code complete pending the conductor's next `make live-test-m1` run
  with the tuned barge onset and cumulative outbound accounting. Real-phone M1 remains
  blocked by the Twilio trial/outbound-DTMF/controlled IVR+hold prerequisites; C1 and the
  real-phone M0 exit remain blocked as before.


### 2026-11-17 — C3 conductor live-gate session (Europe/Vienna)

- Ran the multi-agent flow for C3: Architect (`kimi-code/k3.1-pro`) → Coder
  (`deepseek-official/deepseek-flash`) → Reviewer (`kimi-code/k3.1-pro`). Merged PR #3
  (`feat/c3-call-runner-agent`), PR #4 (`fix/c3-live-gate`), and PR #5
  (`fix/c3-cleanup-timeout`) via squash.
- **Real-Mistral no-phone live gate PASS (observed):** `LIVE_MISTRAL=1 make live-test-m0`
  → `TestLiveM0NoPhone` **PASS** (32.3 s): 591 scripted canonical frames → real STT final →
  dialogue `capture_info` → `finish_call` → real TTS; `patient.reason == "Kontrolle"`
  persisted, terminal `aborted`, `attempts=1`, 756 non-silent 640-byte outbound frames, one
  hangup. Satisfies plan §8 exit criterion 5.
- **Live-gate debugging (conductor-instrumented):** the first live runs exposed three real
  bugs, each fixed by the Coder and merged: (1) STT/TTS realtime readers waited for a
  provider close frame after `transcription.done`/`speech.audio.done`; (2) the scripted
  utterance + missing German→field mapping made the model skip `capture_info`; (3) the agent
  `CleanupTimeout` (5 s) was tighter than the adapters' 15 s `finalDrainLimit`. All recorded
  in `plans/04-m0-e2e.md` §13.
- **Status:** C3 → ✅ code complete. DESIGN's real-phone M0 exit remains blocked by the
  Twilio trial (`<Stream>` stripped); `live-test-m0-phone` compiles/skips and is ready for
  the upgraded-account run.
- **Environment:** no new env keys/deps; the real `MISTRAL_API_KEY` was read only for the
  no-phone live gate (value never printed). Builds used a `/tmp` GOCACHE/GOMODCACHE in this
  sandbox.

### 2026-11-17 — C3 implementation session (Europe/Vienna)

- Implemented [plans/04-m0-e2e.md](plans/04-m0-e2e.md) end to end **without running any
  live test** (per explicit instruction). Committed on `feat/c3-call-runner-agent` and PR #3
  opened against `main`; **not merged**.
- **New core packages:**
  - `internal/core/agent/`: `agent.go` (Config/New/Engine/Run, turn loop, limits),
    `gate.go` (bounded PCM16LE RMS/pre-roll/start/min/silence/max utterance delimiter),
    `transcribe.go` (one STT stream per utterance, 20 ms/frame replay, exactly one final),
    `speak.go` (lazy TTS, verbatim text, concurrent `SendAudio` drainer, Close ordering),
    `tools.go` + `booking_tools.go` (strict tool schemas/decoding/results, per-call
    proposal/confirmation/capture/escalation state, booked-outcome consistency),
    `prompt.go` (fixed German policy + deterministic order JSON + greeting),
    `schema.go` (canonical outcome schema), `errors.go`; 92.9 % statements covered.
  - `internal/core/runner/`: `runner.go` (claim `scheduled→dialing`/attempts++, one
    call-lifetime `Dial`, event-driven transitions, optional `StreamReady`, agent handoff),
    `events.go` (post-answer relay + stable sentinels), `outcome.go` (outcome precedence and
    fixed fallbacks), `shutdown.go` (active-call cancellation/wait); 90.3 % covered.
- **Wiring/lifecycle:** `internal/app` constructs one stateless agent + one runner, exposes
  `App.Agent`/`App.Runner`, and `App.Close` drains the runner. `cmd/insteadly` closes the
  runner **before** `http.Server.Shutdown` on both the signal and listener-error paths.
  No new HTTP route, env key, `Summary` key, or module dependency.
- **Tests:** exhaustive core fakes (no adapter imports); `internal/app/m0_integration_test.go`
  composes the real C2 STT/TTS/dialogue adapters against loopback fake WS/SSE Mistral
  servers plus fake telephony and proves the booked chain (dial/events/statuses → canonical
  inbound → STT final → propose/confirm execution + exact results → streamed dialogue/TTS →
  non-silent canonical outbound → booked Finish/Hangup/attempts=1), plus
  callback_needed/failed/aborted and remote busy/no-answer variants.
- **Live harnesses (compile/skip only; NOT run):** `m0_live_test.go`
  (`TestLiveM0NoPhone`, double-gated on `LIVE_MISTRAL=1` + `MISTRAL_API_KEY`) and
  `m0_phone_live_test.go` (`TestLiveM0Phone`, additionally `LIVE_M0_PHONE=1` +
  `LIVE_TWILIO=1` + `TEST_PHONE` + full Twilio/tunnel config). New Makefile targets
  `live-test-m0` and `live-test-m0-phone`; both excluded from `verify`/CI.
- **Automated verification (observed):**
  - `/Library/Developer/CommandLineTools/usr/bin/make verify` → gofmt/vet clean,
    `go test -race -count=1 ./...` all packages `ok`, `OK: core imports no adapters`,
    binary built.
  - `golangci-lint v2.15.0` (from `/tmp`) → **0 issues**.
  - `make cover` → **87.0 %** total; **agent 92.9 %**, **runner 90.3 %**.
  - `go test -race -count=10 ./internal/core/agent ./internal/core/runner ./internal/app`
    → green.
  - `go test -tags live -count=1 -run 'TestLiveM0(NoPhone|Phone)$' -v ./internal/app`
    → both **SKIP** without gates.
  - `go mod tidy` + `git diff --exit-code -- go.mod go.sum` → no drift; still exactly
    `github.com/coder/websocket v1.8.15`.
  - `git diff main -- internal/core/domain internal/core/ports internal/httpapi docs/api.md`
    → empty.
  - No-env boot: `/healthz` 200 (`stub`/`local`), `POST /api/v1/orders` 201 `created`,
    `POST /api/v1/orders/abc/call` → 404 (no call route), SIGTERM → `shutting down`,
    exit 0.
- **Deviations (also in plan §13):** no live test was run (conductor runs the no-phone
  gate), so C3 is 🔵 not ✅; test-only constructor knobs `agent.Config.Sleep`,
  `agent.Config.CleanupTimeout`, and `runner.Config.ReadinessTimeout`; an active run
  canceled by `Runner.Close` returns both `ErrClosed` and `context.Canceled`; the gate
  discards sub-minimum noise and keeps listening, while the "please repeat" prompt is for
  an empty **final transcript**; `App` exposes both `Agent` and `Runner`.
- **Environment:** no new `.env`/`INSTEADLY_*` keys and no new module dependencies; the
  real key/phone/Twilio config were not read, and no live test or phone call was run. The
  sandbox needed `GOCACHE=/tmp/insteadly-go-cache` for Go builds/tests.
- **Post-review fixes (PR #3):** added `agent.LimitError`/`agent.IsLimit` so listen/
  silence timeout, repeated empty transcript, max turns, max tool rounds, and the agent's
  own max-call duration map to `failed/other` (provider deadlines stay `failed/error`);
  made the post-answer relay cancelable and drain-until-channel-close with an awaited
  shutdown on every exit (stack-trace leak test); fixed the `StreamReady` timer so events
  cannot reset the 20 s bound; rejected JSON `null`/wrong-type optional tool arguments and
  raw invalid UTF-8 in `capture_info`; wrapped the lost same-`Get` claim race with
  `domain.ErrInvalidTransition`; asserted the exact integration STT byte sequence and
  broader log hygiene; fixed the docs `.env` wording, plan whitespace, and the live-test
  failure text. New tests include `TestRunAgentConversationLimitsMapToFailedOther`,
  `TestRunProviderDeadlineIsFailedError`, `TestRunClaimLostAfterGetWrapsInvalidTransition`,
  `TestRunReadinessTimeoutNotResetByEvents`, `TestEventRelayShutdownStopsBlockedDrain`,
  `TestEventRelayDrainsUntilChannelClose`, `TestRunSilentHangupDoesNotLeakRelay`,
  `TestOptionalToolFieldsRejectNullAndWrongTypes`,
  `TestCaptureInfoRejectsNullAndInvalidUTF8`, and the agent limit assertions. No live test
  was run; C3 remains 🔵.
- **Post-review verification (observed):** `make verify` green (gofmt/vet, all race tests
  `ok`, `OK: core imports no adapters`, binary built); `golangci-lint v2.15.0` → **0
  issues**; `make cover` → **87.3 %** total, **agent 93.3 %**, **runner 93.2 %**;
  `go test -race -count=10 ./internal/core/agent ./internal/core/runner ./internal/app` →
  green; both live tests **SKIP**; `go mod tidy` no drift (still only
  `github.com/coder/websocket v1.8.15`); contract drift empty; `git diff --check
  main...HEAD` clean (plan whitespace fixed).
- **Live-gate follow-up (conductor's live run on `main` = `da3741d` failed):** fixed two
  independent causes on `fix/c3-live-gate` without running any live test:
  1. the STT/TTS realtime readers waited for a provider close frame after
     `transcription.done`/`speech.audio.done`, which with a ~9 s utterance produced
     `stt close: context deadline exceeded`; both now treat `done` as terminal (emit final /
     flush tail, `CloseNow`, return), with fake-socket regression tests that keep the
     connection open after `done`;
  2. the live scripted utterance was ambiguous (STT heard "Grundkontrolle") and the model
     skipped `capture_info`; the script now uses the explicit "Der Grund lautet Kontrolle"
     phrasing and the prompt lists the German→`capture_info` field mapping.
  Recorded in `plans/04-m0-e2e.md` §13; C3 remains 🔵 until the conductor's re-run passes.
- **Live-gate follow-up verification (observed):** `make verify` green (gofmt/vet, all race
  tests `ok`, `OK: core imports no adapters`, binary built); `go test -race -count=10
  ./internal/adapters/stt/mistral ./internal/adapters/tts/mistral ./internal/core/agent
  ./internal/core/runner ./internal/app` green; both live tests **SKIP**;
  `golangci-lint v2.15.0` → **0 issues**; `go mod tidy` no drift; contract drift empty;
  `make cover` → **87.4 %** total.
- **Residual live-gate fix (diagnostic run on `main` = `d6d7425` passed the chain but
  exposed a timeout ordering bug):** the agent's default `CleanupTimeout` was 5 s, shorter
  than the STT/TTS adapters' 15 s `finalDrainLimit`, so provider-latent end-of-input drain
  could be preempted with a bare `context deadline exceeded` → `failed/error`. The agent
  default is now **20 s** with a comment requiring it to exceed the adapter's 15 s bound;
  the runner's 5 s cleanup timeout (hangup/finalization only) is unchanged.
  `TestConfigDefaultsAndValidation` asserts the resolved zero-config timeout is ≥ 15 s.
  Recorded in `plans/04-m0-e2e.md` §13; no live test was run; C3 remains 🔵 pending the
  conductor's re-run.
- **Cleanup-timeout fix verification (observed):** `make verify` green (gofmt/vet, all race
  tests `ok`, `OK: core imports no adapters`, binary built); `go test -race -count=10
  ./internal/core/agent ./internal/core/runner ./internal/app` green; both live tests
  **SKIP**; `golangci-lint v2.15.0` → **0 issues**; `go mod tidy` no drift; contract drift
  empty; `git diff --check main...HEAD` clean.

### 2026-11-17 — C2 implementation session (Europe/Vienna)

- Implemented [plans/03-mistral-ai.md](plans/03-mistral-ai.md) end to end; committed on
  `feat/c2-mistral-ai` (PR opened in this session).
- **New packages** `internal/adapters/{stt,tts,dialogue}/mistral`:
  - `stt/mistral`: `adapter.go` (Config/New), `realtime.go` (WS client, bounded audio
    queue, cumulative partials, final-on-close), `batch.go` (multipart WAV→transcript),
    `wav.go`, `language.go`, `errors.go` + fake-server tests.
  - `tts/mistral`: `adapter.go`, `realtime.go`, `offline.go`, `audio.go` (float32 LE 24 kHz
    → 15-tap anti-alias + 3:2 polyphase → PCM16 16 kHz, 640-byte framing), `sse.go`,
    `errors.go` + unit/fake tests.
  - `dialogue/mistral`: `adapter.go`, `protocol.go`, `session.go` (history, pending calls,
    one-turn-at-a-time), `stream.go` (SSE, fragmented tool calls), `outcome.go`
    (finish_call), `sse.go`, `errors.go` + unit/fake tests.
- **Config/wiring:** nested `config.Mistral` + 8 new `INSTEADLY_MISTRAL_*` entries, strict
  validation, secret-safe `Summary`; `internal/app` wires all three with aliases
  `sttmistral`/`ttsmistral`/`dialoguemistral`. `make live-test-mistral` target (excluded
  from `verify`).
- **Automated verification (observed):**
  - `/Library/Developer/CommandLineTools/usr/bin/make verify` → gofmt/vet clean,
    `go test -race -count=1 ./...` all packages `ok`, `OK: core imports no adapters`,
    binary built.
  - `go test -race -count=10 ./internal/adapters/{stt,tts,dialogue}/mistral` → green.
  - `make cover` → **83.9 %** total; mistral packages **stt 83.9 %**, **tts 85.2 %**,
    **dialogue 89.5 %**.
  - `go mod tidy` + `git diff --exit-code -- go.mod go.sum` → no dependency drift.
  - `git diff main -- internal/core/domain internal/core/ports` → empty (no contract drift).
  - `go test -tags live -run '^TestLiveMistral$' ./internal/app` without gates → SKIP.
- **Live smoke (user-authorized):** `LIVE_MISTRAL=1 make live-test-mistral` →
  **PASS** — TTS `Guten Tag.` → 40 canonical 640-byte frames → realtime STT final transcript
  containing `guten` → one dialogue `capture_info` call (not executed). The plan's #1 risk
  (raw `pcm` float32 LE mono 24 kHz encoding) was **confirmed correct on the first attempt**.
- **Amendments (also in the plan §13):** Mistral closes both realtime WebSockets without a
  clean `1000` close frame after `speech.audio.done` / `transcription.done`; the TTS and STT
  readers now treat any close after `done` as success. Transport robustness only — not a
  PCM/model/message-shape mismatch.
- **Docs:** README status/config/layout/roadmap, `docs/architecture.md`, new
  `docs/mistral.md`, PROGRESS, `plans/03-mistral-ai.md` §13 amendments.
- **Environment:** key present under unprefixed `MISTRAL_API_KEY` (value never read/printed);
  app reads only `INSTEADLY_MISTRAL_API_KEY`. No new module deps.

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
  is read only by the build-tagged live test. `MISTRAL_API_KEY` (unprefixed) is present in
  `.env`; the app reads only `INSTEADLY_MISTRAL_API_KEY`, and only the build-tagged
  `LIVE_MISTRAL=1` smoke reads `MISTRAL_API_KEY` directly.
- **Tunnel:** `cloudflared` 2026.11.0 works from `/tmp` (quick tunnel; HTTP + WSS verified);
  it is **not installed** globally. The tunnel URL must equal `INSTEADLY_HTTP_PUBLIC_URL`
  (scheme + host, no trailing slash) or Twilio webhook signatures fail with 403.
- **C1 webhook surface:** `/webhooks/telephony/twilio/{voice,status,stream,stream-status}/{callID}`,
  mounted only when `telephony.provider=twilio`. Status callbacks require a valid
  `X-Twilio-Signature`; Twilio does **not** sign the TwiML `Url` fetch (verified live), so
  the voice route requires a known call ID plus matching `AccountSid`/`CallSid`. The WSS
  handshake is validated by AccountSID + call ID + media format. Trial accounts strip
  `<Stream>`; live audio needs an upgraded account.
- **C3 execution surface:** `internal/core/runner.Runner.Run` is the only invocation path;
  there is **no** public place-call/status endpoint. `internal/core/agent` owns the M0
  energy/silence gate (640 B frames, RMS≥500, 200 ms pre-roll, 30-frame end silence,
  15 s max utterance, 20 s listen timeout) and exactly four executed tools; `finish_call`
  stays adapter-intercepted. The full rules live in `docs/agent.md`. The credential-free
  fake-Mistral chain is the CI proof; `make live-test-m0` (real Mistral, no phone) is the
  remaining C3 gate and `make live-test-m0-phone` stays deferred behind the Twilio upgrade.

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
| 7 | Coarse `DialogueEngine` port — adopt now or later | **Resolved in C3:** not adopted; the fine-grained STT/TTS/Dialogue ports were sufficient. Revisit only for a real managed-pipeline adapter. |

