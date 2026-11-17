# C5 — Orchestration (M2): Postgres order store, scheduler, status API + SSE, place-call endpoint

> Status: plan (read-only once implementation starts; amend via dated `## 13. Amendments` only).
> Contract source of truth: `DESIGN.md` §2.1.1/§2.1.2/§2.2/§2.4/§5/§6; on conflict, DESIGN wins.
> Seams inherited from C3/C4: `plans/04-m0-e2e.md` §11, `plans/05-conversation.md` §11.

---

## 1. What this chunk is

C5 is the **M2 — orchestration** chunk. It turns the single-binary prototype into a
durably-coordinated system: an order store backed by Postgres (replacing the local
JSON-file store as the durable source of truth when opted in), an in-process **scheduler**
that picks up eligible orders and drives them through `Runner.Run` (opening windows, quiet
hours, retries, callback requeue), a **status API** with **SSE** streaming for the future
web dashboard, and the **place-call endpoint** that C3/C4 deliberately deferred.

Two facts are already established and are treated as given, not re-decided:

1. **`callback_needed` is a terminal `Outcome` on a `finished` order, not an order status.**
   "Callback rescheduling" is therefore an explicit **requeue** operation: re-open a
   `finished` order into a fresh `scheduled` attempt **while preserving history** — it is
   *not* a status transition in the spine state machine. §3.4 defines the requeue
   operation and its guardrails.
2. **`Runner.Run` already owns the atomic `scheduled → dialing` transition and the
   `Attempts++` claim** (see `internal/core/runner/runner.go` lines 111–123). The scheduler
   must **never** mutate an order to `dialing`. It selects/claims eligibility and hands a
   `scheduled` order to `Runner.Run`, which remains the final duplicate-dial fence.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | `ports.SchedulerStore` contract (list eligible, atomic lease claim, release) | `internal/core/ports/` |
| 2 | Domain scheduling surface: `NextAttemptAt`, `AttemptHistory`, `Order.Schedule`, `Order.Requeue` with guardrails | `internal/core/domain/` |
| 3 | Scheduler loop: ticker + injectable fake clock, eligibility, lease claim, bounded concurrent `Runner.Run`, outcome→retry/requeue policy, opening windows + quiet hours (CET/CEST) | `internal/core/scheduler/` |
| 4 | Core in-memory event hub for order status changes | `internal/core/events/` |
| 5 | Postgres adapter: `ports.OrderStore` + `ports.SchedulerStore`, embedded-SQL migrations, JSONB order mapping | `internal/adapters/storage/postgres/` |
| 6 | Local adapter gains `ports.SchedulerStore` (in-process lease) for parity; blobs stay local | `internal/adapters/storage/local/` |
| 7 | Status API + SSE routes, error envelope reuse, place-call endpoint with API-key auth + idempotency | `internal/httpapi/` |
| 8 | Composition: scheduler + event hub + Postgres pool + SSE hub construction/close; graceful shutdown ordering | `internal/app/`, `cmd/insteadly/` |
| 9 | Config: Postgres DSN (secret), scheduler params, opening windows, API key, idempotency TTL; secret-safe `Summary` | `internal/core/config/` |
| 10 | Credential-free fake-clock scheduler tests + fake-telephony integration + Docker/testcontainers-gated Postgres tests | core/adapter tests |
| 11 | Docs and PROGRESS update (implementation session only) | `README.md`, `docs/`, `PROGRESS.md` |

### Out of scope (do NOT implement — later chunks)

- **River or any external job queue.** DESIGN §2.1.2 says "River before production"; the
  prototype scheduler is deliberately in-process. §4.2 documents the rationale and the C7
  seam.
- **Clinic directory / praxisplan.at / per-clinic opening hours.** DESIGN §6.3 (Q3 clinic
  directory) is deferred. C5 uses a single config-supplied opening-window/quiet-hours
  schedule (config/JSON, not hardcoded) applied to every order; per-clinic windows are a
  C6/C7 concern with their own data-source/licensing decision.
- **Payments, notifications, web UI.** No Stripe webhook, no email/SMS, no React. The
  place-call endpoint makes an order eligible to dial *now* (it drives `created → paid →
  scheduled`), but it does not implement payment capture.
- **Recording/transcript/audio persistence and recording-flag enforcement.** That stays
  C7 (per `plans/05-conversation.md` §11). **Blobs remain on the local adapter in C5** even
  when orders are on Postgres; the Postgres blob store is explicitly not built.
- **Human transfer / new telephony or AI providers / second Postgres provider (e.g.
  pgx→database/sql, another driver).** No ORM; pgx only (§3.1).
- **AuthN beyond a single static API key.** Real users/roles/tokens belong to C6 (web app).
- **Horizontal runner scaling / active-call draining under multiple processes.** The
  Postgres store is multi-process-safe by construction; verifying multi-instance behavior is
  a test-only goal, not a deployment claim (see §9).

---

## 2. Preconditions and baseline

1. **Baseline observed while planning (2026-11-17, Europe/Vienna):**
   ```bash
   cd /Users/q/Insteadly
   git rev-parse --short HEAD        # → 0b0dd07
   git status --short --branch       # clean on main
   GOCACHE=/tmp/gocache-insteadly GOMODCACHE=/tmp/gomodcache-insteadly \
     go test -count=1 ./...
   ```
   Result: **all packages `ok`** (`cmd/insteadly` and `internal/core/ports` report "no test
   files"; every other package passes). Re-run before implementation; do not implement C5 on
   a red baseline.
2. **Docker** available for the Postgres-gated tests: `docker version` reported
   **Docker Desktop 4.86.0 / Engine 29.7.2**, server running. The default `make verify`
   must still pass *without* Docker — see §7 for the exact gate.
3. **Read the executable contracts first:** `internal/core/ports/{storage,errors,telephony,
   dialogue,speech,notify,payments}.go`, `internal/core/domain/{order,ids,call}.go`,
   `internal/core/runner/{runner,shutdown,outcome}.go`, `internal/adapters/storage/local/*`,
   `internal/httpapi/{server,orders}.go`, `internal/core/config/{config,env}.go`,
   `internal/app/{app,providers}.go`, `cmd/insteadly/main.go`, and the adapter error
   surfaces `internal/adapters/{stt,tts,dialogue}/*/errors.go` +
   `internal/adapters/telephony/twilio/client.go`.
4. **Toolchain facts (from PROGRESS.md):** `make` → use
   `/Library/Developer/CommandLineTools/usr/bin/make`; sandbox may need
   `/tmp` GOCACHE/GOMODCACHE; `golangci-lint` is optional/absent locally (CI installs
   v2.15.0). `go.mod` has exactly one dependency (`github.com/coder/websocket v1.8.15`,
   two `go.sum` lines). Any further dependency is a plan-level decision — §3.1 makes that
   decision.

---

## 3. Ports → components and exact contracts

### 3.1 Postgres adapter and the storage decision (decided)

**Decision: keep `ports.Storage`/`ports.OrderStore` unchanged and add one new minimal port
`ports.SchedulerStore`. The Postgres adapter implements `ports.OrderStore` + the new
`ports.SchedulerStore`; it does NOT implement `ports.BlobStore` in C5.**

Rationale:

- `ports.OrderStore` is the exact surface the runner and the HTTP API already consume
  (`Get`/`Create`/`List`/`Update`). Changing it would ripple through the runner and every
  test fake for no benefit. `OrderStore.Update(ctx, id, fn)` is already the correct
  optimistic/atomic shape for the runner's claim and for scheduler requeue.
- The scheduler additionally needs two operations that the generic store does not express
  and that are *cross-process* concerns: **list due/eligible orders** and **atomically
  lease-claim** one without touching its `status`. Those belong on a narrow, consumer-defined
  port (`SchedulerStore`), exactly as DESIGN §2.2 prescribes (each port is the minimal
  surface the consumer needs).
- Blobs stay on the local adapter. Recording/transcript persistence is C7; the `BlobStore`
  side of `ports.Storage` therefore remains satisfied by `local` in every C5 configuration
  (see the composite in §3.5). This avoids building a Postgres bytea/large-object blob store
  that C7 will likely supersede with S3/MinIO anyway.
- **No ORM, no second driver.** `pgx/v5` + `pgxpool` only. It is the maintained, low-level
  driver for PostgreSQL with first-class `FOR UPDATE SKIP LOCKED` support via plain SQL and
  native JSONB round-tripping; an ORM (gorm/sqlc/ent) adds a codegen/mapping layer with no
  benefit at this schema size. A second provider (e.g. `lib/pq` or `database/sql`) would
  violate the "one provider" rule and buy nothing.

**New dependencies (plan-level justification, exact pins):**

| Module | Version (pinned) | Justification |
|---|---|---|
| `github.com/jackc/pgx/v5` (incl. `pgxpool`) | `v5.13.0` (latest at planning, verified via `proxy.golang.org/github.com/jackc/pgx/v5/@latest` → `v5.13.0`, published 2026-11-11) | PostgreSQL driver + connection pool. Pure-Go adapter package only; core never imports it. Pulls a small, well-known transitive set (`pgpassfile`, `pgservicefile`, `puddle`, `golang.org/x/crypto`, `golang.org/x/text`). |
| `github.com/testcontainers/testcontainers-go` + `.../modules/postgres` | `v0.45.0` (latest at planning, verified via the proxy → `v0.45.0`, published 2026-11-05) | Test-only: spin up an ephemeral `postgres:16-alpine` container for the Postgres adapter/integration tests. Docker is available locally; the tests are gated so the default `make verify` stays green without Docker (§7). |

`go.sum` impact: pgx adds ~6–8 lines (direct + transitives); testcontainers-go + the
postgres module add a larger transitive set (docker client, moby types, `dario.cat/mergo`,
etc.) — roughly 80–150 `go.sum` lines. All are **test/build-time only** for the C5 runtime
path (the shipped binary's runtime deps grow only by pgx + its small transitive set). This
is the last large dependency expansion planned before C6; flag any further dependency in
C6+ the same way.

### 3.2 Domain additions (verbatim contract)

Two new `domain.Order` fields and two new methods. These are **additive**: the existing
JSON tags use `omitempty` where present, so old local JSON files still decode, and
`Order.Transition` / `Order.Finish` are untouched.

```go
// domain/order.go — additions only (existing fields/methods unchanged)

// AttemptRecord is one completed dial attempt in the order's durable history. It is
// append-only and is what makes "callback rescheduling" a history-preserving requeue
// rather than a status transition.
type AttemptRecord struct {
    Number    int          `json:"number"`               // 1-based; equals the order.Attempts at that attempt's claim
    Outcome   Outcome      `json:"outcome"`              // the terminal outcome of that attempt
    StartedAt time.Time    `json:"started_at"`           // when the attempt was claimed (dialing)
    EndedAt   time.Time    `json:"ended_at"`             // when the attempt finished
}

// Order gains:
NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty"` // due time for the next dial; nil/zero = due immediately (scheduled only)
AttemptHistory []AttemptRecord `json:"attempt_history,omitempty"` // prior finished attempts, oldest first
```

```go
// Schedule marks a paid order eligible to dial at next. It is the only way an order
// enters "scheduled" except Requeue. next may be nil (due immediately) or a future time.
func (o *Order) Schedule(next *time.Time) error {
    if o.Status == OrderScheduled {
        return fmt.Errorf("%w: already scheduled", ErrInvalidTransition)
    }
    if o.Status != OrderPaid {
        return fmt.Errorf("%w: schedule from %q", ErrInvalidTransition, o.Status)
    }
    o.Status = OrderScheduled
    o.NextAttemptAt = next
    o.UpdatedAt = time.Now()
    return nil
}
```

```go
// Requeue re-opens a finished order into a fresh scheduled attempt, preserving history.
// It is the ONLY path from "finished" back to "scheduled"; the spine transition table is
// deliberately not extended (see order.go "Retry-from-failed is a C5 scheduler concern").
//
// Guardrails, enforced here and again by the scheduler (§4.3):
//   1. Only a FINISHED order may be requeued.
//   2. The finished outcome must be retryable: Kind == callback_needed (CallbackAt set), or
//      Kind == failed with a FailReason the scheduler policy classifies as retryable.
//      booked and aborted are never requeued here.
//   3. callback_needed requires a non-zero CallbackAt in the future.
//   4. The caller (scheduler) enforces the attempt cap; Requeue records history but does
//      not itself decide whether another attempt is allowed.
func (o *Order) Requeue(next *time.Time) error {
    if o.Status != OrderFinished || o.Outcome == nil {
        return fmt.Errorf("%w: requeue requires a finished order with an outcome", ErrInvalidTransition)
    }
    switch o.Outcome.Kind {
    case OutcomeCallbackNeeded:
        if o.Outcome.CallbackAt == nil || o.Outcome.CallbackAt.IsZero() {
            return fmt.Errorf("%w: callback_needed requires callback_at", ErrInvalidOutcome)
        }
    case OutcomeFailed:
        // FailReason retryability is scheduler policy; Requeue only demands a reason.
        if o.Outcome.FailReason == "" {
            return fmt.Errorf("%w: failed requires fail_reason", ErrInvalidOutcome)
        }
    default:
        return fmt.Errorf("%w: outcome %q is not requeueable", ErrInvalidTransition, o.Outcome.Kind)
    }

    o.AttemptHistory = append(o.AttemptHistory, AttemptRecord{
        Number:    o.Attempts,
        Outcome:   *o.Outcome,
        StartedAt: o.UpdatedAt, // last persisted transition time; best-effort start evidence
        EndedAt:   time.Now(),
    })
    o.Outcome = nil
    o.Status = OrderScheduled
    o.NextAttemptAt = next
    o.UpdatedAt = time.Now()
    return nil
}
```

Note: `StartedAt` in `AttemptRecord` is best-effort (`UpdatedAt` at requeue time); we do
not add a dedicated `DialedAt` column in C5 because the runner already stamps `UpdatedAt`
on `scheduled → dialing`. If operators later need exact per-attempt dial timestamps, that
is a small dated amendment, not a silent schema change.

### 3.3 `ports.SchedulerStore` (verbatim, new)

```go
// ports/storage.go — additions

// ErrNotEligible is returned by SchedulerStore.Claim when the order is no longer
// claimable (not due, already claimed by another worker, or no longer in an eligible
// status). The scheduler treats it as a benign miss, never an error to surface.
var ErrNotEligible = errors.New("not eligible")

// SchedulerStore is the narrow surface the scheduler consumes (DESIGN §2.2). It is
// deliberately separate from OrderStore: listing and lease-claiming due orders is a
// cross-process coordination concern that the generic CRUD store does not express.
//
// A claim is a LEASE, not a status change. Claim never moves an order to "dialing";
// the runner's own scheduled→dialing Update remains the single duplicate-dial fence.
type SchedulerStore interface {
    // ListEligible returns up to limit orders that are due now and not leased by
    // another worker. Eligible means: status == paid, OR (status == scheduled AND
    // (next_attempt_at IS NULL OR next_attempt_at <= now)), AND (lease_until IS NULL
    // OR lease_until <= now). Orders are returned in a stable order (next_attempt_at,
    // then id) so repeated calls are deterministic under a fixed clock.
    ListEligible(ctx context.Context, now time.Time, limit int) ([]domain.Order, error)

    // Claim atomically leases one eligible order to owner until leaseUntil, returning
    // the leased order. It returns ErrNotEligible (never ErrNotFound) when the order
    // is no longer claimable; the caller retries another candidate. The order's
    // status is left unchanged. Claim must be safe to call from many processes.
    Claim(ctx context.Context, id domain.OrderID, owner string, leaseUntil time.Time) (domain.Order, error)

    // Release clears a lease held by owner, best-effort. It is idempotent and never
    // returns an error for a missing/already-released order. Release exists so a
    // worker that drops an order (e.g. Runner.Run rejects it) does not burn the full
    // lease; lease expiry remains the crash-safety net.
    Release(ctx context.Context, id domain.OrderID, owner string) error
}
```

`ListEligible`/`Claim`/`Release` are the *only* scheduler-specific store calls. Requeue and
`paid → scheduled` are expressed as ordinary `OrderStore.Update` callbacks invoking the
domain methods above — the scheduler needs no dedicated "reschedule" store verb, which
keeps the port minimal and lets both stores share the same atomic `Update` path.

### 3.4 Requeue operation and guardrails (the two given facts, made explicit)

`callback_needed` is terminal and the runner leaves the order `finished`. To act on it, the
scheduler performs a **requeue**:

```
finished (outcome=callback_needed, callback_at=T)  ──Requeue──►  scheduled (next_attempt_at=T), outcome=nil, history+=attempt
```

Same shape for a retryable `failed` outcome, with `next_attempt_at = now + backoff`
(§4.3). Guardrails (enforced at two layers):

1. **Domain (`Order.Requeue`, §3.2):** only `finished` + a retryable outcome; `booked` and
   `aborted` are rejected; `callback_needed` requires a future `callback_at`.
2. **Scheduler policy (§4.3):** the attempt cap (`max_attempts`) and the
   `fail_reason → retryable/terminal` table decide *whether* to requeue at all; the
   scheduler never requeues `booked`/`aborted`, never infers policy from free-text
   `Outcome.Detail`, and never requeues past the cap. Exceeding the cap leaves the order
   `finished` (terminal `failed`), it does not loop.

History is preserved in `Order.AttemptHistory` (append-only). `Order.Attempts` remains the
monotonic total (runner-owned increment), so the scheduler can compare `Attempts` against
`max_attempts` without trusting history length.

### 3.5 Event hub (core, new)

SSE needs a decoupled source of order-status changes. Add a tiny in-process hub in core
(no HTTP, no external deps):

```go
// core/events/hub.go — new package internal/core/events

// OrderEvent is one observed order status change. Status is the post-change status;
// Outcome is set only when the event is a finish (or the requeue clears it, signaled by
// status=scheduled with outcome=nil). IDs are domain IDs; no secrets or patient detail.
type OrderEvent struct {
    OrderID  domain.OrderID   `json:"order_id"`
    Status   domain.OrderStatus `json:"status"`
    Outcome  *domain.Outcome  `json:"outcome,omitempty"`
    Attempts int              `json:"attempts"`
    At       time.Time        `json:"at"`
}

// Publisher is what the runner and scheduler see; Subscriber is what the SSE layer sees.
type Publisher interface {
    Publish(ctx context.Context, ev OrderEvent) error // non-blocking: drops on full buffer, counts drop
}

type Subscriber interface {
    Subscribe(ctx context.Context) (<-chan OrderEvent, func(), error)
}

type Hub interface {
    Publisher
    Subscriber
    Close() error
}
```

- `NewHub(buffer int) *Hub` with a bounded per-subscriber channel (default 64); a slow
  subscriber's channel is dropped (closed) rather than blocking the publisher — SSE clients
  reconnect and re-snapshot, so a dropped tail is safe.
- The runner publishes an `OrderEvent` after each persisted status change (claim → dialing,
  ringing, in-call, and the terminal finish). This is a **new optional field on
  `runner.Config`** (`Events Publisher`, nil-safe in tests) — no other runner behavior
  changes; the runner never imports the concrete hub.
- The scheduler publishes for `paid → scheduled` (Schedule), requeue, and terminal/exhausted
  decisions. The HTTP API only *subscribes* to the hub; it never publishes.

The local `ports.OrderStore.Update` path is unchanged; events are emitted by the runner and
scheduler (the two components that drive transitions), never by the store, so a store
adapter needs no event awareness.

### 3.6 Postgres schema (DDL, migration 0001)

Migration approach: **embedded SQL + a trivial versioned runner, no external tool.** Each
migration is an `//go:embed` `.sql` file in the adapter package, applied inside a
transaction, guarded by a `schema_migrations(version int primary key, applied_at)` table.
This needs no `golang-migrate`/`goose` dependency and keeps `make check-deps` trivial. The
runner is ~40 lines; the adapter exposes `Migrate(ctx) error`, called from `app.New` when
`storage.provider == "postgres"`.

```sql
-- 0001_orders.sql (embedded as migrations/0001_orders.sql)
CREATE TABLE IF NOT EXISTS orders (
    id                text PRIMARY KEY,             -- domain.OrderID, 32 lowercase hex
    status            text NOT NULL,                -- domain.OrderStatus
    clinic            jsonb NOT NULL,
    patient           jsonb NOT NULL,
    outcome           jsonb,                        -- NULL until finished
    recording         jsonb,                        -- per-order overrides; NULL = global
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    attempts          integer NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz,                  -- NULL = due immediately (scheduled only)
    attempt_history   jsonb NOT NULL DEFAULT '[]'::jsonb,
    lease_owner       text,                         -- scheduler worker id while leased
    lease_until       timestamptz,                  -- lease expiry; NULL = not leased
    CONSTRAINT orders_status_check CHECK (status IN
      ('created','paid','scheduled','dialing','ringing','in_call','finished'))
);

-- Eligibility index: the scheduler's hot query. Partial index only over the two
-- claimable statuses so it stays small as finished orders accumulate.
CREATE INDEX IF NOT EXISTS orders_scheduler_due_idx
    ON orders (next_attempt_at, id)
    WHERE status IN ('paid','scheduled');

CREATE INDEX IF NOT EXISTS orders_created_at_idx ON orders (created_at DESC, id DESC);
```

`Order` ↔ row mapping (adapter-internal, no new domain types):

| Column | Go |
|---|---|
| `id` | `string(OrderID)` |
| `status` | `string(Status)` |
| `clinic` | `json.Marshal(Order.Clinic)` → store as `[]byte` (jsonb) |
| `patient` | `json.Marshal(Order.Patient)` |
| `outcome` | `json.Marshal(Order.Outcome)`; `NULL` when nil |
| `recording` | `json.Marshal(Order.Recording)`; `NULL` when nil |
| `created_at`/`updated_at`/`next_attempt_at` | `time.Time` (pgx maps timestamptz↔time.Time with the session time zone set to `UTC`) |
| `attempts` | `int` |
| `attempt_history` | `json.Marshal([]domain.AttemptRecord)` |
| `lease_owner`/`lease_until` | adapter-local scheduling columns, **not** part of `domain.Order` |

`OrderStore` semantics on Postgres:

- `Create`: `INSERT … ON CONFLICT (id) DO NOTHING`; when 0 rows affected → wrap
  `ports.ErrConflict`. Assign `NewOrderID()` when `ID == ""`; stamp `CreatedAt`/`UpdatedAt`;
  force `Status = created` (mirrors local).
- `Get`: `SELECT …`; `pgx.ErrNoRows` → wrap `ports.ErrNotFound`.
- `List`: `ORDER BY created_at DESC, id DESC` (mirrors local tie-break).
- `Update`: **one transaction**: `SELECT … FOR UPDATE` (row lock), decode, run `fn`,
  re-encode, `UPDATE`, `COMMIT`. This gives cross-process atomicity (serialized by the row
  lock) and preserves the exact `Update(ctx, id, fn)` semantics the runner relies on.
  `fn` returning an error → rollback and return it wrapped. The runner's
  `scheduled → dialing` check still runs inside this transaction, so it is the same
  duplicate-dial fence it is on the local store.

`SchedulerStore` on Postgres:

```sql
-- ListEligible
SELECT … FROM orders
WHERE status IN ('paid','scheduled')
  AND (next_attempt_at IS NULL OR next_attempt_at <= $now)
  AND (lease_until IS NULL OR lease_until <= $now)
ORDER BY next_attempt_at NULLS FIRST, id
FOR UPDATE SKIP LOCKED
LIMIT $limit;
```

```sql
-- Claim (one transaction)
BEGIN;
  SELECT … FROM orders WHERE id=$id FOR UPDATE;
  -- re-check eligibility under the lock; else rollback + return ports.ErrNotEligible
  UPDATE orders SET lease_owner=$owner, lease_until=$lease_until
   WHERE id=$id
     AND status IN ('paid','scheduled')
     AND (next_attempt_at IS NULL OR next_attempt_at <= $now)
     AND (lease_until IS NULL OR lease_until <= $now);
  -- 0 rows updated → rollback + ports.ErrNotEligible
COMMIT; -- return the leased order
```

```sql
-- Release
UPDATE orders SET lease_owner=NULL, lease_until=NULL
 WHERE id=$id AND lease_owner=$owner;
```

`FOR UPDATE SKIP LOCKED` is the key: concurrent workers each skip rows another transaction
has locked, so N workers claim disjoint orders with no advisory-lock bookkeeping and no
status mutation.

**BlobStore on Postgres:** `internal/adapters/storage/postgres` exposes `Orders()`
(`ports.OrderStore`) and `Scheduler()` (`ports.SchedulerStore`). It does **not** expose
`Blobs()`. Composition (§3.8) keeps blobs on the local adapter.

### 3.7 Local adapter `SchedulerStore` (parity, in-process)

The local store implements the same `ports.SchedulerStore` so the scheduler is
provider-agnostic and single-process development is fully exercised:

- `ListEligible`: under the existing `RWMutex`, scan `orders/`, select `paid` or
  `scheduled && (next_attempt_at nil/<= now)`, exclude leased (`lease_until > now`), sort by
  `next_attempt_at` then `id`, cap at `limit`.
- `Claim`: under the write lock, re-check eligibility, record `lease_owner`/`lease_until`
  in an in-memory map keyed by order ID (not persisted), return the order; else
  `ports.ErrNotEligible`.
- `Release`: delete the lease entry.

The lease is in-memory only in local mode — correct, because the local store is
single-process; the runner's status check is the only fence it needs. Persisted
`next_attempt_at`/`attempt_history` are written through the normal JSON file (new
`omitempty` fields).

### 3.8 Composition (`internal/app`)

`App` gains: `Scheduler *scheduler.Scheduler`, `Events events.Hub`, and (in postgres mode)
a closeable `pgxpool.Pool` handle. `App.Storage` stays `ports.Storage` for the API/runner.

```go
// providers.go — buildStorage becomes two builders:
func buildStorage(c config.Storage) (ports.Storage, ports.SchedulerStore, io.Closer, error)
```

- `local`: one `*local.Store` serves as `ports.Storage`, `ports.SchedulerStore`; `io.Closer`
  is a no-op.
- `postgres`: construct a `*postgres.Store` (order store + scheduler store), `Migrate`,
  then return a composite `ports.Storage{ Orders: pgStore, Blobs: localBlobs }` (blobs on
  `local.New(c.Local.Dir)`), `SchedulerStore: pgStore`, and a closer that closes the pool.
  `c.Local.Dir` is still required (it hosts blobs), so the existing local-dir validation
  stays.

`app.New` then wires `scheduler.New(scheduler.Config{Store: schedStore, Runner: run,
Events: hub, Clock: scheduler.SystemClock{}, …})` and stores the hub on `App` for
`httpapi` to subscribe to.

---

## 4. Scheduler specification

### 4.1 Loop

In-process, one goroutine, started by `app.New` (or `App.Start()` — see §7/§8), stopped by
`Scheduler.Close`:

```
for {
  wait until next tick OR ctx.Done()
  free := maxConcurrent - inFlight()
  if free <= 0 { continue }
  now := clock.Now()
  due := store.ListEligible(ctx, now, free)          // snapshot, no status change
  for _, o := range due {
      if !window.Allows(now) { continue }            // do not even claim outside windows
      claimed, err := store.Claim(ctx, o.ID, workerID, now+lease)
      if errors.Is(err, ports.ErrNotEligible) { continue }
      if o.Status == paid {
          // paid → scheduled (domain.Schedule) via store.Update, publish event
          claimed, err = store.Update(ctx, claimed.ID, func(o *domain.Order) error {
              return o.Schedule(&now)                 // due immediately
          })
      }
      go s.runOne(claimed)                            // bounded by maxConcurrent
  }
}
```

`runOne(order)` calls `runner.Run(ctx, order.ID)`, then on return reads the finished order,
classifies its outcome (§4.3), and either requeues (via `Update` + `Order.Requeue`), leaves
it terminal, or publishes the terminal/exhausted event. On shutdown the loop context is
canceled, in-flight `runner.Run` calls get canceled → the runner persists `aborted` (which
is terminal, so no requeue), and `Scheduler.Close` waits for them (§8).

**Cross-process safety:** the lease claim is atomic (Postgres row lock); `Runner.Run`'s own
`Update` then atomically does `scheduled → dialing` + `Attempts++`. Two workers can both
lease-claim different orders, but even in the race where a lease expires and a second
worker claims the *same* order, only one `scheduled → dialing` transition can win — the
other `Runner.Run` returns the existing "status is not scheduled" error and the worker
releases the lease. The scheduler therefore **never** mutates to `dialing`.

### 4.2 Clock, windows, quiet hours

- **Clock:** `scheduler.Clock` interface `{ Now() time.Time }` with `SystemClock` and a
  test `FakeClock` (`Set/Advance`). All `now` values come from the clock; the ticker is the
  only wall-clock consumer and is short-circuited in tests (see §7).
- **Location:** fixed `Europe/Vienna` (CET/CEST) via `time.LoadLocation`, injected so tests
  can pin it. DST-correct by construction because all window math uses the loaded location.
- **Windows:** a config-provided list `[]Window{ Dow time.Weekday; Start, End string /* "HH:MM" */ }`
  parsed in `config` and handed to the scheduler. Default (no env): Mon–Fri 08:00–18:00
  Europe/Vienna (see §5). The scheduler computes "next valid slot ≥ (now + minBackoff)" by
  scanning forward day by day.
- **Quiet hours:** expressed as the *complement* of the windows (no separate quiet-hours
  list in C5 — a quiet period is simply a time with no window). This is simpler and
  identical in effect; a future per-clinic directory can layer on top without changing the
  scheduler.
- **Opening-window source decision:** config/JSON, **not** hardcoded, because the clinic
  directory is Q3 (DESIGN §6.3) and hardcoding Austrian reception hours in core would bake a
  policy we explicitly deferred. Config keeps no-env boot green while making the policy
  explicit and swappable.

### 4.3 Outcome → retry/requeue mapping (exact table)

The scheduler reads the finished order's `Outcome.Kind` + `Outcome.FailReason`. Adapters'
`Temporary()`/`RetryDelay()` are **metadata only** and are never read by the scheduler in
C5 (the runner already collapsed them into a normalized `fail_reason`; C3 §11 said provider
classification "can inform C5, never C3 auto-retry" — C5 chooses its own policy table and
does not re-import adapter types).

| `Outcome.Kind` | `FailReason` (failed only) | Action | Next attempt |
|---|---|---|---|
| `booked` | — | **terminal** (success). No requeue. | — |
| `aborted` | — | **terminal**. No requeue (covers user cancel and shutdown-canceled calls). | — |
| `callback_needed` | — | **requeue** if `Attempts < max_attempts`; else terminal `failed`(reason=`attempts_exhausted`). | `callback_at` (from outcome), clamped to the next window |
| `failed` | `busy` | **requeue** if under cap. | `now + busy_backoff` (`busy_backoff` default 15m), then next window |
| `failed` | `no_answer` | **requeue** if under cap. | `now + no_answer_backoff` (default 30m), then next window |
| `failed` | `ivr_dead_end` | **requeue** if under cap (different menu state may resolve). | `now + no_answer_backoff` |
| `failed` | `rejected` | **terminal** (the clinic actively refused; do not spam). | — |
| `failed` | `error` | **requeue** if under cap (transient provider/dial/conversation error). | `now + error_backoff` (default 5m), then next window |
| `failed` | `other` | **terminal** in C5 (unexplained end before an outcome; do not infer). See note. | — |
| `failed` | any other | **terminal** (unknown reason is not retried). | — |

Notes:

- **`failed/other` is terminal in C5.** The runner emits `other` for "ended before a
  dialogue outcome" and a few limit cases. Retrying an unexplained outcome risks re-dialing
  a human on a call that may have reached someone; the conservative default is to stop and
  surface it. If real-phone evidence later shows a safe retryable sub-class, add a dated
  amendment with the exact `detail`/reason pair — never key off free text.
- **Backoff shape:** fixed per-class delay + a per-attempt multiplier
  `delay * attempts` (linear, capped by `max_attempts`), then advanced to the next opening
  window. This avoids an unbounded exponential tail on a 3-attempt cap and is trivially
  deterministic under the fake clock. No jitter in the core (determinism for tests); jitter
  belongs in a production River worker if ever needed.
- **Cap:** `max_attempts` counts *dial attempts* (the runner's `Attempts`). Default 3.
  Reaching the cap on a retryable outcome leaves the order `finished`; the scheduler
  publishes a terminal event and never loops. `Attempts` includes the just-failed attempt,
  so `Attempts >= max_attempts` ⇒ no further requeue.

### 4.4 Parameters (defaults; all overridable via env, §5)

| Parameter | Default | Meaning |
|---|---|---|
| `tick` | 5s | scheduler scan interval (wall clock; fake clock short-circuits tests) |
| `max_attempts` | 3 | total dial attempts incl. the first |
| `max_concurrent` | 8 | in-flight `Runner.Run` calls |
| `lease` | 2m | claim lease duration (≫ longest expected call, `call timeout` 6m ⇒ reclaim only after crash, not during a live call) |
| `busy_backoff` | 15m | |
| `no_answer_backoff` | 30m | |
| `error_backoff` | 5m | |
| `windows` | Mon–Fri 08:00–18:00 Europe/Vienna | opening windows |

`lease` (2m) vs `call timeout` (6m): the lease only guards *eligibility selection*, not the
live call. A worker that crashes mid-call has its lease expire while the call is still
running, but the runner's status check still blocks a duplicate dial until that call
actually ends (and if the process is dead, the telephony webhook path is also dead — a C7
deployment/recovery concern, noted in §9, not a C5 correctness bug for the single-process
prototype).

---

## 5. Config and environment surface

New `config` structs (`Scheduler`, `Postgres`, `API`) and registry entries. `storage.provider`
gains a third value `postgres`. The `Summary()` map adds only **non-secret** scheduler/API
values and a **redacted** Postgres marker — it must never log the DSN or its password.

```go
type Storage struct {
    Provider string // "local" | "postgres"
    Local    Local
    Postgres Postgres
}

type Postgres struct {
    DSN string // secret; full libpq/URL connstring (host, db, user, password, sslmode)
}

type Scheduler struct {
    Enabled         bool
    Tick            time.Duration
    MaxAttempts     int
    MaxConcurrent   int
    Lease           time.Duration
    BusyBackoff     time.Duration
    NoAnswerBackoff time.Duration
    ErrorBackoff    time.Duration
    WindowsJSON     string // JSON array: [{"dow":"mon","start":"08:00","end":"18:00"}, …]
}

type API struct {
    Key           string        // secret; placeholder auth for place-call (C6 replaces with real auth)
    IdempotencyTTL time.Duration
}
```

Registry additions (strict-mode; `INSTEADLY_` prefix, dots→underscores):

| Env var | Type | Secret | Default |
|---|---|---|---|
| `INSTEADLY_STORAGE_PROVIDER` | string | no | `local` (now also accepts `postgres`) |
| `INSTEADLY_POSTGRES_DSN` | string | **yes** | empty (required when provider=postgres) |
| `INSTEADLY_SCHEDULER_ENABLED` | bool | no | `true` |
| `INSTEADLY_SCHEDULER_TICK` | duration | no | `5s` |
| `INSTEADLY_SCHEDULER_MAX_ATTEMPTS` | int | no | `3` |
| `INSTEADLY_SCHEDULER_MAX_CONCURRENT` | int | no | `8` |
| `INSTEADLY_SCHEDULER_LEASE` | duration | no | `2m` |
| `INSTEADLY_SCHEDULER_BUSY_BACKOFF` | duration | no | `15m` |
| `INSTEADLY_SCHEDULER_NO_ANSWER_BACKOFF` | duration | no | `30m` |
| `INSTEADLY_SCHEDULER_ERROR_BACKOFF` | duration | no | `5m` |
| `INSTEADLY_SCHEDULER_WINDOWS` | string | no | `[{"dow":"mon","start":"08:00","end":"18:00"},…fri]` |
| `INSTEADLY_API_KEY` | string | **yes** | empty (place-call auth disabled when empty — dev only) |
| `INSTEADLY_API_IDEMPOTENCY_TTL` | duration | no | `10m` |

Validation additions:

- `postgres` provider ⇒ `INSTEADLY_POSTGRES_DSN` non-empty and parseable (use
  `pgxpool.ParseConfig` in `app.New`/adapter constructor, not in `config` — config only
  checks non-empty and trims). `config` must not import pgx (keep `config` dependency-free).
- Scheduler durations/ints > 0; `max_attempts >= 1`; `max_concurrent >= 1`; `lease > 0`.
- `windows` JSON must parse to ≥1 window with `HH:MM` start < end and a valid `dow`
  (`mon..sun`).
- `api.idempotency_ttl` > 0.
- **No-env boot stays green:** provider `local`, scheduler enabled, defaults above, empty
  API key (auth off) — `go run ./cmd/insteadly` still boots exactly as today.
- `Summary()`: add `storage.provider`, `scheduler.enabled/tick/max_attempts/max_concurrent/
  lease/backoffs/windows`, and `postgres.dsn` → the literal string `<redacted>` (or
  `<set>` vs `<unset>`); **never** the raw DSN. `api.key` is `secret`, never summarized.

Duration parsing: add a `durationSetter` to `env.go` using `time.ParseDuration`, matching
the existing `strSetter`/`intSetter`/`boolSetter` pattern.

---

## 6. Status API + SSE and the place-call endpoint

### 6.1 Routes

| Method/Path | Purpose | Auth |
|---|---|---|
| `GET /api/v1/orders` | unchanged (list, newest first) | none |
| `POST /api/v1/orders` | unchanged (create `created` order) | none |
| `GET /api/v1/orders/{id}` | unchanged (full order incl. status/outcome/history) | none |
| `GET /api/v1/orders/{id}/events` | **new** — SSE stream for one order | none in C5 (prototype; C6 adds web auth) |
| `POST /api/v1/orders/{id}/call` | **new** — make an order eligible to dial now | **API key** (Bearer) when `INSTEADLY_API_KEY` set |

Error envelope is the existing `{"error":{"code":…,"message":…}}` (`httpapi/server.go`);
`writeStoreError` is reused verbatim.

### 6.2 Place-call endpoint (decided)

**Decision:** `POST /api/v1/orders/{id}/call` is an **eligibility** endpoint, not a direct
dial. It drives `created → paid → scheduled(next_attempt_at = now)` and lets the scheduler
pick it up. Rationale: (a) DESIGN §2.4 routes *all* dialing through the scheduler, (b) it
preserves the "scheduler never mutates to dialing / `Runner.Run` is the only dial path"
invariant, and (c) it makes the endpoint idempotent and side-effect-free at the runner
layer. A separate synchronous "dial right now" escape hatch would reintroduce a second
dial path for zero C5 value.

- **Request:** empty body (or `{}`). Optional `Idempotency-Key` header (≤128 chars).
- **Auth:** `Authorization: Bearer <key>` (constant-time compare against
  `INSTEADLY_API_KEY`). When the configured key is empty, auth is **disabled** (dev) and a
  warning is logged once at startup. `401 {"code":"unauthorized"}` on mismatch. This is
  explicitly a *prototype* placeholder; real user auth/tokens are C6 and this must not be
  dressed up as security.
- **Behavior / idempotency:** in-process idempotency map `key → (orderID, status)`, TTL =
  `api.idempotency_ttl` (default 10m), cleaned lazily. Reusing a key returns the **stored
  result** (`200` + order) without re-scheduling. Missing/different key, normal path:
  1. load order (`404` if missing);
  2. if already `paid`/`scheduled`/`dialing`/… or `finished` → `409` (or `200` idempotent if
     key matches) with the current order and code `conflict`/`already_scheduled`; never
     dial twice;
  3. if `created` → `Update` with `MarkPaid`-then-`Schedule` (the paid transition is a
     placeholder for C6's payment gate; C5 exposes no paid endpoint, so `call` is the only
     way to reach `scheduled`);
  4. `202 Accepted` with the `scheduled` order and `Location` header. The scheduler's next
     tick claims and runs it.
- **Idempotency key not persisted** in C5 (in-memory only) — documented as prototype scope;
  a durable key store is a C6 payment-idempotency concern.

Domain note: add a small `MarkPaid()` (or reuse `Transition(OrderPaid)` from `created`) —
no new method needed, the existing transition table already has `created → paid`. The
handler composes `Update{Transition(paid)}` then `Update{Schedule(now)}` as one callback
sequence (two `Update` calls are fine; the endpoint is idempotent by key, and a crash
between them leaves `paid`, which the scheduler itself also schedules — see §4.1).

### 6.3 SSE (`GET /api/v1/orders/{id}/events`)

- `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`,
  `X-Accel-Buffering: no`. Flush after every event via `http.NewResponseController` (the
  existing `statusRecorder` already `Unwrap()`s, so `http.Flusher`/`ResponseController`
  reach the real writer).
- **Snapshot-then-stream:** on subscribe, send one `order` event with the current full
  order (initial state), then stream `order` events as they arrive from the hub for that
  order ID. Client filters to its ID; a future `GET /api/v1/orders/events` (all orders) is
  a trivial C6 addition, not built now.
- **Event wire shape** (SSE framing: `event: order\ndata: <json>\n\n`):

  ```json
  {"order_id":"…","status":"scheduled","outcome":null,"attempts":1,"at":"2026-11-17T20:00:00+01:00"}
  ```

  `outcome` is present only on terminal events; `retry: 3000` is sent once at subscribe so
  clients reconnect after 3s. `Last-Event-ID` is accepted but no replay buffer is kept in
  C5 (subscribers always get a fresh snapshot, which makes replay redundant); document this.
- **Heartbeat:** a `: ping` comment line every 15s to defeat idle proxies; on heartbeat the
  server also checks `r.Context().Done()` and returns when the client is gone.
- **Backpressure:** the hub already drops a slow subscriber's tail (closes its channel);
  the SSE handler then ends the stream with a final `event: error` + `{"code":"overrun"}`
  and closes — the client reconnects and re-snapshots, so no event is permanently lost.

The SSE handler is mounted in `httpapi.New` with the hub from `App.Events`; it bypasses the
JSON `writeJSON`/error-envelope path (SSE has its own framing), but still flows through the
requestID/logger/recoverer middleware.

---

## 7. Implementation steps (do them in this order; each has a verify command)

> Use `/Library/Developer/CommandLineTools/usr/bin/make` where `make` is needed; sandbox
> builds use `GOCACHE=/tmp/gocache-insteadly GOMODCACHE=/tmp/gomodcache-insteadly`.

1. **Domain additions** (`NextAttemptAt`, `AttemptHistory`, `AttemptRecord`, `Schedule`,
   `Requeue`). No behavior change to existing transitions/Finish.
   - Verify: `go test -count=1 ./internal/core/domain/` + new unit tests for
     `Schedule`/`Requeue` guardrails (each rejected case).
2. **`ports.SchedulerStore` + `ports.ErrNotEligible`** in `ports/storage.go`; add a doc
   comment noting it is scheduler-only.
   - Verify: `go build ./internal/core/ports/`.
3. **`internal/core/events`** hub (`Hub`/`Publisher`/`Subscriber`), bounded channels, drop
   counting, `Close`. Core-only (stdlib + domain).
   - Verify: `go test -count=1 ./internal/core/events/`.
4. **Local adapter `SchedulerStore`** (in-memory lease + `ListEligible`/`Claim`/`Release`).
   - Verify: `go test -count=1 ./internal/adapters/storage/local/`.
5. **Runner event hook** — add optional `Events events.Publisher` to `runner.Config`
   (nil-safe); publish `OrderEvent` after claim/dialing, ringing, in-call, finish. No other
   runner change; `runner.Run`/`Finish` invariants untouched.
   - Verify: `go test -race -count=1 ./internal/core/runner/` (existing + a new
     "publisher receives events" test with a fake publisher).
6. **Scheduler** (`internal/core/scheduler`): `Clock`, windows/location, loop, claim,
   `runOne`, retry/requeue policy, concurrency cap, `Close`. Core-only + stdlib + slog.
   - Verify: `go test -race -count=1 ./internal/core/scheduler/` (fake clock suite).
7. **Config** — structs, registry entries, duration setter, windows parse, validation,
   redacted `Summary`.
   - Verify: `go test -count=1 ./internal/core/config/` (new validation/default/secret
     tests); confirm no env still boots (`INSTEADLY_` unset ⇒ provider local, scheduler
     enabled).
8. **Postgres adapter** (`internal/adapters/storage/postgres`): migration runner + embedded
   `0001_orders.sql`, `OrderStore` (Create/Get/List/Update with `FOR UPDATE`), JSONB
   mapping, `SchedulerStore` (`SKIP LOCKED`), `Close`. Depends on `pgx/v5`.
   - Verify: `go build ./internal/adapters/storage/postgres/`; adapter unit tests that
     don't need a live DB (SQL-free helpers) run in `go test`.
9. **Dependency pinning** — `go get github.com/jackc/pgx/v5@v5.13.0`; then
   `go get github.com/testcontainers/testcontainers-go@v0.45.0` +
   `github.com/testcontainers/testcontainers-go/modules/postgres@v0.45.0` (test files
   only). Record the resulting `go.mod`/`go.sum` deltas.
   - Verify: `go mod tidy && go build ./...`; `make check-deps` still `OK`.
10. **Postgres-gated integration tests** (`internal/adapters/storage/postgres/*_test.go`
    + an `internal/app/m2_integration_test.go` using testcontainers): CRUD round-trip,
    JSONB fidelity, `FOR UPDATE` claim contention (two goroutines), `SKIP LOCKED` disjoint
    claims, migration idempotence, retry/requeue through a real Postgres-backed scheduler.
    Gated by the `postgres` build tag (§7a).
    - Verify: `go test -tags postgres -count=1 ./internal/adapters/storage/postgres/ ./internal/app/`
      with Docker running; **without Docker**, the same command with the gate disabled
      (tag off) compiles nothing and `go test ./...` stays green.
11. **HTTP API** — place-call handler (+ idempotency map, API-key middleware), SSE handler
    + hub wiring, route registration, `Location`/`202`/error mapping.
    - Verify: `go test -race -count=1 ./internal/httpapi/` (httptest: auth 401/200/409,
      idempotent replay, SSE snapshot + streamed event via a fake hub).
12. **Composition + lifecycle** (`internal/app`, `cmd/insteadly`) — build storage composite,
    scheduler/hub/pool wiring, `App.Start()`/`Close()`, `main.go` shutdown ordering (§8).
    - Verify: `go build ./...`; `go test -race -count=1 ./internal/app/` (constructor +
      lifecycle tests with local storage and a fake clock); `go run ./cmd/insteadly` boots
      and `/healthz` returns `storage: local`.
13. **Docs + PROGRESS** (implementation session only): `docs/architecture.md` (add
    orchestration), `README.md` (new env table), `PROGRESS.md` session entry. No plan edits
    except dated Amendments.
    - Verify: `make verify` (full: lint, race test, check-deps, build) — green without
      Docker and without credentials; `make test-postgres` green with Docker (optional).

### 7a. Exact gate for Postgres tests (decided)

**Build tag `postgres`** for every test that needs a live database:

```go
//go:build postgres
```

- Default `go test ./...` and `make verify` compile **none** of the Docker-dependent code,
  so they stay green with no Docker installed and never require testcontainers at runtime.
- A `make test-postgres` target runs `go test -tags postgres -count=1 ./...` and requires
  Docker (documented). In CI, `make verify` is the credential/Docker-free gate; a separate
  CI job (optional) runs `make test-postgres` on Docker-enabled runners.
- The Postgres **adapter's non-test code is NOT build-tagged** — it compiles always (pgx is
  a normal module dependency) so `go build ./...` type-checks the wiring and the shipped
  binary can select `postgres` at runtime. Only the tests that spin up a container are
  tagged. Rationale: keeps `go mod tidy` simple (no tag-dependent require set) and keeps the
  production build honest.

---

## 8. Wiring and lifecycle

`cmd/insteadly/main.go` changes only in the shutdown path (the happy-path construction
stays `app.New(cfg)` + `httpapi.New` + `ListenAndServe`):

```
config.Load() → app.New(cfg) → app.Start(ctx) [starts scheduler goroutine]
  → httpapi.New (mounts hub-backed SSE + place-call) → ListenAndServe
on SIGTERM/SIGINT:
  1. Scheduler.Close(ctx): stop ticker, stop issuing, cancel loop ctx, wait in-flight
     runOne goroutines (they see ctx canceled; Runner persists aborted).
  2. App.Close(ctx): Runner.Close — drains/aborts any remaining active calls (idempotent;
     scheduler already stopped issuing).
  3. Events hub Close (stops accepting subscribers; SSE handlers still drain).
  4. http.Server.Shutdown (drain in-flight HTTP; SSE handlers observe closed hub and end).
  5. Postgres pool Close (only after runner/scheduler are done).
```

`App.Start(ctx)` is new and idempotent; `App.Close(ctx)` is extended to run the above in
order (Scheduler → Runner → Hub → Pool), each bounded by the caller's deadline. Pool close
must be **last** so a persisting runner/scheduler never touches a closed pool.

---

## 9. Test matrix (file → cases)

| File | Cases |
|---|---|
| `internal/core/domain/order_test.go` (extend) | `Schedule` only from `paid`, sets `next_attempt_at`; `Schedule` rejects `created`/`scheduled`/`finished`; `Requeue` from `finished`+`callback_needed` preserves history and clears outcome; `Requeue` rejects `booked`/`aborted`/missing `callback_at`/non-finished; `AttemptHistory` append ordering. |
| `internal/core/events/hub_test.go` | publish→subscribe delivery; multiple subscribers each get the event; slow subscriber drop (buffer overflow) counts and closes; `Close` ends subscriptions; nil-context publish no-op. |
| `internal/adapters/storage/local/scheduler_test.go` | `ListEligible` selects paid + due scheduled, excludes leased/future `next_attempt_at`, sorts deterministically; `Claim` leases and returns order, second claim ⇒ `ErrNotEligible`; `Release` clears; expired lease is re-eligible. |
| `internal/core/runner/events_test.go` (extend) | runner publishes dialing/ringing/in-call/finished events in order with correct statuses/attempts; nil publisher is a no-op; events still emitted on failure/abort paths. |
| `internal/core/scheduler/scheduler_test.go` | **fake clock** — paid order gets scheduled+claimed+run; retryable `busy` reschedules `next_attempt_at=now+15m`; `no_answer`/`ivr_dead_end`/`error` per table; `rejected`/`other`/`booked`/`aborted` terminal; `callback_needed` requeues at `callback_at`; cap ⇒ terminal `failed(attempts_exhausted)`; outside-window due order is not claimed; concurrency cap respected; lease release on runner rejection; `Close` cancels loop and waits in-flight. |
| `internal/core/scheduler/windows_test.go` | window parse, DST boundary (Europe/Vienna), "next window ≥ now+backoff", weekend/quiet period, empty windows ⇒ nothing claimed. |
| `internal/core/config/config_test.go` (extend) | new env vars parsed; durations; windows JSON invalid ⇒ error; `postgres` requires DSN; `local` default boots with no env; `Summary` never contains DSN/API key (`postgres.dsn` = `<redacted>`/`<unset>`). |
| `internal/httpapi/orders_test.go` (extend) | place-call: missing auth 401, bad key 401, ok key + `created` ⇒ 202 + scheduled, already-scheduled ⇒ 409, idempotent replay ⇒ 200 same order, idempotency TTL expiry ⇒ re-schedules; SSE: snapshot then streamed event, heartbeat, client disconnect cleanup, slow-client overrun ends stream. |
| `internal/adapters/storage/postgres/orders_test.go` (`//go:build postgres`) | testcontainers: Create/Get/List round-trip, JSONB fidelity (nil vs empty, nested structs, `attempt_history`), `Update` row-lock serialization, `Create` conflict ⇒ `ErrConflict`, `Get` miss ⇒ `ErrNotFound`; migration idempotent (run twice). |
| `internal/adapters/storage/postgres/scheduler_test.go` (`//go:build postgres`) | `ListEligible` due/lease filtering; `Claim` atomic under two goroutines (exactly one wins, other `ErrNotEligible`); `SKIP LOCKED` disjoint claims across N workers; expired lease re-eligible; `Release`. |
| `internal/app/m2_integration_test.go` (local + `//go:build postgres` variant) | end-to-end with **stub telephony/agent**: create → place-call → scheduler (fake clock, local store) → runner runs → outcome persists; a `busy` fake-telephony session retries once then `booked` on second attempt (history has two entries, `attempts=2`); callback_needed requeue at `callback_at`; SSE subscriber observes the status sequence. |
| `internal/app/app_test.go` (extend) | constructor: local vs postgres composite (postgres path uses testcontainers under `postgres` tag); `Close` ordering closes pool last; `Start` idempotent. |

Fakes: scheduler tests use a `fakeStore` implementing `SchedulerStore` and a `fakeRunner`
(records calls, returns scripted outcomes) so **no live phone/cloud is ever needed**;
`internal/app/m2_integration_test.go` reuses the existing `integrationTelephony` scripting
pattern from `m0_integration_test.go` (`session.end(domain.EndBusy)` etc.). No Twilio, no
Mistral in any C5 test.

---

## 10. Definition of done

**C5 code complete** (the milestone exit that C5 owns — *not* the DESIGN real-phone exits,
which remain blocked by the Twilio trial and are not C5's to unblock):

- [ ] `make verify` green **without Docker and without credentials**: `gofmt`, `go vet`,
  race tests, `make check-deps` (`internal/core` imports no adapter), `go build ./...`.
- [ ] `go test -count=1 ./...` green with the `postgres` tag off (default).
- [ ] `make test-postgres` green with Docker (testcontainers `postgres:16-alpine`):
  Postgres order-store + scheduler-store round-trips and claim-contention tests pass.
- [ ] `go run ./cmd/insteadly` with **no `INSTEADLY_*` env** boots, `/healthz` reports
  `storage: local`, scheduler enabled; with `INSTEADLY_STORAGE_PROVIDER=postgres` +
  `INSTEADLY_POSTGRES_DSN=…` it migrates, stores orders in Postgres, and blobs still land
  on local disk.
- [ ] The scheduler, under a fake clock, drives a created order through place-call →
  scheduled → runner → finished, including one `busy` retry and one `callback_needed`
  requeue with preserved `attempt_history`.
- [ ] The runner's single-attempt exactly-once boundary is **unchanged and still the only
  `scheduled → dialing` transition** — proven by a test that two concurrent claims of the
  same order result in exactly one dial.
- [ ] SSE streams a snapshot then live status events; place-call is idempotent and
  API-key-gated when a key is configured.
- [ ] `config.Summary()` and startup logs never contain the DSN, its password, or the API
  key.

**Explicitly NOT C5 done:** DESIGN's real-phone M0/M1 exits (blocked by the Twilio trial
stripping `<Stream>` and unproven outbound DTMF); real Twilio/Mistral calls; per-clinic
directory data; multi-instance deployment. These are unchanged blockers or later chunks.

---

## 11. Risks and decision gates

1. **`failed/other` terminality.** If the first real run shows "ended before a dialogue
   outcome" is safely retryable in practice, that is a dated amendment naming the exact
   `detail`/`reason`, not a silent retry. Gate: real-phone evidence.
2. **Lease vs call-timeout mismatch.** A lease (2m) shorter than a call (6m) means a second
   process could re-claim a crashed worker's order mid-call; the runner's status check is
   the fence, but multi-instance recovery under a live call is a C7 deployment concern.
   Gate: keep `lease < call_timeout` out of scope for single-process C5; do not tune the
   runner's 6m timeout in C5.
3. **`pgx`/`testcontainers` are the first large dependency additions since C1.** The
   justification is in §3.1; if `go mod tidy` surfaces an unexpected large transitive set,
   stop and re-evaluate (e.g. pin a narrower testcontainers version) rather than silently
   committing a bloated `go.sum`. Gate: any new dependency in C6+ repeats this justification.
4. **API-key auth is a placeholder.** Do not present it as production security; C6 owns real
   auth. Gate: if the web app lands before C6 auth, revisit — but C5 must not build a user
   system.
5. **Opening windows config vs clinic directory.** C5 uses one global window list; the
   moment per-clinic hours arrive (Q3), the scheduler's window source becomes an interface
   (`WindowProvider`) — leave that seam (§12) rather than hardcoding the config type into
   every scheduler test.
6. **`make verify` without Docker.** The `postgres` build tag (§7a) is the guarantee. If a
   future test accidentally imports testcontainers without the tag, `make verify` breaks —
   enforce with a review rule: any `testcontainers` import must sit behind `//go:build
   postgres`.
7. **`config` must stay dependency-free.** Parse/validate the DSN in the adapter, not in
   `config` (only non-empty/trim in `config`), so `config` never imports pgx. `make
   check-deps` is the guard.

---

## 12. Size estimate and C6 seams

**Size:** ~2,600–3,400 LOC across new packages (scheduler ~600, events ~150, postgres
adapter ~700 incl. migrations, httpapi SSE/place-call ~450, config ~250, domain ~120) plus
~1,600–2,200 LOC of tests. Comparable to C3/C4; the risky part is the Postgres
claim/`SKIP LOCKED` correctness, not volume.

**C6 seams (do not build now, but leave room):**

- `WindowProvider` interface (so per-clinic hours from the Q3 directory replace the global
  config list without touching the scheduler loop).
- `Payments` port + webhook: the place-call endpoint's `created → paid` step is the obvious
  seam where a real Stripe webhook later replaces the manual "call now" transition.
- A durable idempotency-key store (the in-memory map is a documented placeholder).
- `GET /api/v1/orders/events` (all-orders SSE) and per-user auth replace the static API key.
- River (Postgres-backed queue) as the production replacement for the in-process loop; the
  scheduler's `Clock`/policy are deliberately isolated so a River worker can reuse them.
- A `BlobStore` on Postgres/S3 (C7) to retire the local-blob composite.

---

## 13. Sources

Repository sources read directly: `DESIGN.md` (§2.1.1/§2.1.2/§2.2/§2.4/§5/§6),
`PROGRESS.md` (status, environment facts, open questions §6.2/§6.3),
`plans/04-m0-e2e.md` §11 and `plans/05-conversation.md` §11, `internal/core/ports/storage.go`,
`internal/core/domain/{order,ids,call}.go`, `internal/core/runner/{runner,shutdown,outcome}.go`,
`internal/adapters/storage/local/{local,orders}.go`, `internal/httpapi/{server,orders}.go`,
`internal/core/config/{config,env}.go`, `internal/adapters/{stt,tts,dialogue}/*/errors.go`,
`internal/adapters/telephony/twilio/client.go`, `internal/app/{app,providers}.go`,
`cmd/insteadly/main.go`, `internal/adapters/stub/{telephony,dialogue}.go`, `Makefile`,
`go.mod`, `go.sum`.

External sources actually fetched while planning (2026-11-17):

1. Go module proxy `@latest`/`@v/list` for `github.com/jackc/pgx/v5` (→ `v5.13.0`),
   `github.com/testcontainers/testcontainers-go` (→ `v0.45.0`), and
   `github.com/testcontainers/testcontainers-go/modules/postgres` (→ `v0.45.0`) —
   `<https://proxy.golang.org/…/@latest>`, `<https://proxy.golang.org/…/@v/list>`.
2. [`pkg.go.dev/github.com/jackc/pgx/v5/pgxpool`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
   — `pgxpool.New(ctx, connString)`, `Pool.BeginTx`/`Query`/`Close`, and the
   keyword-value/URL connstring format (pool settings via `pool_max_conns` etc.) used in §3.6.
3. [Testcontainers for Go — Postgres module](https://golang.testcontainers.org/modules/postgres/)
   — `postgres.Run(ctx, "postgres:16-alpine", …)`, `WithDatabase/Username/Password`,
   `ConnectionString()`, `testcontainers.TerminateContainer` used in §7/§9.

---

## 14. Amendments

### 2026-11-17 — C5 implementation decisions (dated, additive)

The implementation followed §7 in order. These are the concrete decisions where the plan
left a seam or was internally ambiguous; none changes a port, state-machine, or security
contract.

1. **`events.Hub` is a concrete struct, not an interface.** §3.5 sketched
   `type Hub interface` *and* `NewHub(buffer int) *Hub`, which cannot both hold. The package
   now exposes `type Hub struct`, `Publisher` and `Subscriber` interfaces, and
   `NewHub`. `App.Events` is `*events.Hub`; consumers depend on `events.Publisher`
   (runner/scheduler) or `events.Subscriber` (HTTP SSE).
2. **Exhausted retries rewrite the outcome.** §4.3 says a capped `callback_needed`
   becomes terminal `failed(attempts_exhausted)` but does not say how a finished order is
   rewritten. The scheduler's `markExhausted` performs one `OrderStore.Update` that sets
   `Outcome = failed/attempts_exhausted` and stamps `UpdatedAt` (status stays `finished`);
   it does not append history, because no requeue occurs. The same rule applies to a
   retryable `failed` at the cap so every exhausted order is visibly terminal.
3. **`Claim` has no `now` parameter.** §3.3 fixes the signature, while §3.6/§3.7 require a
   due-time re-check under the lease lock. Both adapters therefore evaluate the Claim-time
   due check against the wall clock; the scheduler's `ListEligible(now)` snapshot is the
   policy clock. Store-backed scheduler tests pin the fake clock in the past so the
   scheduler's generated due times are still in the wall-clock past; the app-level local
   integration additionally aligns the place-call's wall-clock `next_attempt_at` with the
   fake clock (a test-only step, commented in `m2_integration_test.go`).
4. **Pool close is not inside `App.Close`.** §8 lists "Scheduler → Runner → Hub → Pool" as
   `App.Close`'s order, but HTTP shutdown sits between the hub and the pool. To keep the
   pool last, `App.Close(ctx)` performs scheduler → runner → hub, and a separate idempotent
   `App.CloseStorage()` closes the Postgres pool; `cmd/insteadly` calls it after
   `http.Server.Shutdown`.
5. **Place-call is one atomic `Update`.** §6.2 allows two `Update` calls; the handler uses a
   single `Update` callback (`Transition(paid)` then `Schedule(now)`) so the transition and
   due time are one cross-process-atomic write.
6. **SSE end-of-stream code.** A hub subscription closes both on slow-client overflow and on
   hub shutdown (there is no separate close reason on the channel). The handler emits the
   documented `event: error` / `code: overrun` frame in both cases; clients always
   re-snapshot, and the handler documents the distinction. The wire event uses §3.5's
   `json:"outcome,omitempty"` (so a nil outcome is omitted, not rendered as `null` as in the
   §6.3 example).
7. **Attempt-history length after busy→booked is one, not two.** §9's m2 row says "history
   has two entries" after a busy retry then booked, but §3.2/§3.4 define history as the
   *prior finished* attempts appended by `Requeue`. After the booked second attempt the
   order is terminal and no requeue occurs, so `attempt_history` holds the single busy
   record and `attempts == 2`; the current/booked attempt is the order's `outcome`. The m2
   tests assert that contract and preserve the busy record.
8. **Fake-clock seams.** `Scheduler.Start/Close` follow §8, and `RunOnce`/`Wait` are
   exported so tests can drive deterministic scans without a wall-clock ticker. `New`
   applies the §4.4 defaults for zero values. `App.New` falls back to
   `config.DefaultSchedulerWindows` only for a hand-built `Config` with empty windows;
   `config.Load` still rejects an empty window set.
9. **Internal signature.** `buildStorage` takes a context and logger (not §3.8's
   two-argument sketch) so pool creation/migration is bounded and adapter logs reach the
   process logger. The externally visible constructor remains `app.New(cfg)`.
10. **`m2_integration_test.go` gating.** The local/stub end-to-end test is untagged (it must
    run in the Docker-free default suite); the Postgres composition/scheduler variant is
    `internal/app/m2_postgres_test.go` behind `//go:build postgres`. `make test-postgres`
    runs the tagged suite. Tests run with `CGO_ENABLED=0` locally because this machine's
    Xcode clang is broken; that is an environment workaround, not a code requirement.
