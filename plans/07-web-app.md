# C6 — Web app (M3): React+TS registration, order form, dashboard; Stripe payments; real auth/API

> **Status:** planned, not started. Chunk owner: next coding session.
> **Source of truth:** [DESIGN.md](../DESIGN.md) §2.1/§2.2/§2.3/§2.4/§5(M3)/§6(Q4,Q5) — this plan
> derives from it and must not contradict it; on conflict DESIGN wins (stop and flag).
> **Progress register:** [PROGRESS.md](../PROGRESS.md) — update it at the end of the
> implementation session.
> **Seams inherited from C5:** [plans/06-orchestration.md](06-orchestration.md) §12
> (`Payments` port/webhook replacing the manual `created→paid` step, durable idempotency,
> per-user auth replacing the static API key, `GET /api/v1/orders/events`).
> **Hard constraint (binding):** Twilio/live-phone is **NOT** in scope and must not be
> touched. The web app + payments + auth are the whole C6 scope.

---

## 1. What this chunk is

C6 is the **M3 — web app** milestone: the first frontend chunk. It adds a **React +
TypeScript single-page app** (registration/login, order form, dashboard with live SSE
status) served by the existing Go binary, **real user authentication** (replacing the C5
prototype API-key gate for user-facing routes), and the **Stripe payments adapter** behind
the already-declared `ports.Payments` (Checkout → redirect, signed webhook → `paid`).

The existing order spine is untouched: C6 does **not** change the scheduler, `Runner.Run`,
or `Order.Finish`. It only (a) adds a *user* dimension to orders, (b) makes the payment
webhook the production `created → paid` writer, and (c) serves a UI over the existing
snake_case JSON API.

### In scope (build these)

| # | Deliverable | Where |
|---|---|---|
| 1 | `domain.User` / `domain.Session` / `domain.UserID` + additive `Order.{UserID,PaidAt,PaymentReference}` + `Order.MarkPaid` | `internal/core/domain/` |
| 2 | New ports: `AuthStore` (users + sessions) and `PasswordHasher` | `internal/core/ports/` |
| 3 | `internal/core/auth` service: register / login / logout / session validation, opaque-token + sha256-at-rest (stdlib only) | `internal/core/auth/` |
| 4 | bcrypt password hasher (behind `PasswordHasher`) | `internal/adapters/password/bcrypt/` |
| 5 | `AuthStore` on both stores; `orders.user_id` / `paid_at` / `payment_reference` migration `0002_users.sql`; local parity | `internal/adapters/storage/{local,postgres}/` |
| 6 | Stripe payments adapter + `stub` payments adapter (both implement `ports.Payments`) | `internal/adapters/payments/{stripe,stub}/` |
| 7 | Auth + checkout + payment-webhook HTTP surface, ownership scoping, cookie handling | `internal/httpapi/` |
| 8 | React+TS+Vite SPA (`web/`): register/login/logout, order form, dashboard + live SSE; `go:embed` of the built bundle behind a build tag | `web/` |
| 9 | Composition: wire `App.Payments` / `App.Auth`, SPA static handler | `internal/app/`, `internal/httpapi/`, `cmd/insteadly/` |
| 10 | Config: `auth.*`, `payments.*`/`stripe.*` (secret-safe `Summary`), keep no-env boot green | `internal/core/config/` |
| 11 | Tests: fake-Stripe httptest, auth suites, Postgres-gated users/sessions, Vitest frontend, optional gated Playwright | core/adapter/frontend tests |
| 12 | Docs + PROGRESS (implementation session only) | `README.md`, `docs/`, `PROGRESS.md` |

### Out of scope (do NOT implement — later chunks)

- **Any Twilio/live-phone work.** No dialing changes, no Twilio adapter edits, no live call.
- **Notifications (email/SMS).** `ports.Notifier` stays `nil` until C7.
- **Per-clinic opening hours / clinic directory (Q3).** The C5 global window config is
  unchanged; the `WindowProvider` seam stays for C7.
- **River / a real job queue.** The C5 in-process scheduler is unchanged.
- **Recording/transcript/audio persistence and recording-flag enforcement.** Blobs stay
  local; the Postgres blob store is C7.
- **Refunds, subscription/recurring pricing, multi-price checkouts.** C6 implements one
  fixed-price Checkout Session (or a single configured amount) and the
  `checkout.session.completed` → `paid` bridge only. `PaymentEvent.Refunded` is reserved for C7.
- **CSRF tokens, password reset, email verification, rate-limiting, multi-tenant/roles.**
  Documented as production-hardening seams (§13), not built in C6.

---

## 2. Preconditions and baseline

1. **Baseline observed while planning (2026-11-17, Europe/Vienna):**
   ```bash
   cd /Users/q/Insteadly
   git rev-parse --short HEAD        # → bb7e1b3  (docs: mark C5 code complete)
   git status --short --branch       # clean on main
   GOCACHE=/tmp/gocache-insteadly-c6 GOMODCACHE=/tmp/gomodcache-insteadly-c6 \
     go test -count=1 ./...
   ```
   Result: **all packages `ok`** (exit 0). `cmd/insteadly`, `internal/core/ports`, and
   `internal/adapters/storage/postgres` report "no test files" — the Postgres adapter's
   tests are `//go:build postgres` and are excluded from the default run by design. Re-run
   before implementing; do not implement C6 on a red baseline.
2. **Toolchain (verified this session):** Node **v26.4.0**, npm **11.17.0** (no pnpm);
   `web/` does not exist yet; `.gitignore` currently ignores `/bin/`, `/data/`, `.env`,
   `.idea/`, `*.log`, `/coverage.out`, `/dist/` — the plan adds `web/node_modules/` and
   `web/dist/`.
3. **Read the executable contracts first:** `internal/core/ports/{payments,storage,notify,
   errors}.go`, `internal/core/domain/{order,ids,notify}.go`,
   `internal/httpapi/{server,orders,calls,sse}.go`, `internal/core/config/{config,env}.go`,
   `internal/app/{app,providers}.go`, `cmd/insteadly/main.go`,
   `internal/adapters/storage/{local,postgres}/*`, `Makefile`, `go.mod`.
4. **`make`** → use `/Library/Developer/CommandLineTools/usr/bin/make`; sandbox builds use
   `/tmp` GOCACHE/GOMODCACHE; Postgres-tagged tests need `CGO_ENABLED=0` (broken Xcode
   clang). `go.mod` currently: `coder/websocket v1.8.15`, `pgx/v5 v5.13.0`,
   `testcontainers-go v0.45.0` (+ postgres module, test-only), with
   `golang.org/x/crypto v0.58.0` already present as an **indirect** dependency. Any new
   dependency is a plan-level decision (§3.4).

---

## 3. Design decisions (decided, with rationale)

These are the C6 decisions the conductor/architect asked to settle before coding. Each is
binding on the implementation.

### 3.1 Frontend stack + serving

**Decision: React 19 + TypeScript + Vite, npm, single deployable via `go:embed` behind a
`webembed` build tag. No client router, no state-management library.**

- **React + TS + Vite (npm).** DESIGN §1.1 already fixed "React + TypeScript" for the web
  app; Vite is the standard dev/build tool for a React SPA, has a built-in dev proxy (which
  we use to reach the Go API during development), and emits a plain static `dist/` that a Go
  `embed.FS` can serve. npm (not pnpm) because the toolchain fact says "no pnpm". No
  Next/Remix/Astro: a framework that requires a separate Node server in production buys
  nothing here — the Go binary is already the single server, and the SPA is fully static.
- **No React Router, no Redux/Zustand/etc.** The app has three views (auth → order form →
  dashboard). Navigation is a tiny `useState`-driven view switch plus `fetch`; a router or a
  state mega-library would add dependencies for zero value at this size. React built-ins
  (`useState`/`useEffect`/`useReducer` + one `AuthProvider` context) are sufficient.
- **Project layout `web/`:**
  ```
  web/
    package.json  package-lock.json  vite.config.ts  tsconfig.json  tsconfig.node.json  index.html
    embed.go              // package web; //go:build webembed → //go:embed all:dist
    serve.go              // package web; HTTP handler + SPA fallback (both tags)
    noembed.go            // package web; //go:build !webembed → placeholder handler
    src/
      main.tsx  App.tsx  api.ts  types.ts
      auth/{AuthProvider.tsx,Register.tsx,Login.tsx}
      orders/{OrderForm.tsx,OrderList.tsx,OrderDetail.tsx,Checkout.tsx}
      dashboard/Dashboard.tsx
    dist/                 // Vite output (gitignored)
  ```
- **`go:embed` mechanics (the one subtlety).** `go:embed` patterns cannot contain `..`, so
  the embed must live in a package whose directory contains `dist/`. That package is `web`
  itself (a `.go` file at `web/embed.go` next to `package.json`). To keep `go build ./...`,
  `go vet ./...`, and `make verify` green **with no Node installed**, the embed is opt-in:
  - `web/embed.go` (`//go:build webembed`): `//go:embed all:dist` + `var distFS embed.FS` +
    the SPA file/fallback handler.
  - `web/noembed.go` (`//go:build !webembed`): a handler returning a small static HTML
    page ("web app not built — run `make web` / `npm run build`").
  - `web/serve.go` (always compiled): the public `web.Handler()` that dispatches to whichever
    handler the build tag selected, plus the SPA-fallback logic (serve an existing file from
    the FS, else `index.html`).
  This is the cleanest way to satisfy "single deployable binary" **and** "backend CI stays
  green without npm". The simpler alternative (always `//go:embed all:dist` + commit a
  hand-written `dist/index.html` seed) is acceptable but makes `npm run build` dirty a
  tracked file; the build-tag version is preferred.
- **Build wiring (Makefile).** `make build` becomes `npm ci && npm run build &&
  CGO_ENABLED=0 go build -tags webembed …`. The release workflow therefore always embeds the
  real SPA. `make verify`/`go build ./...` (untagged) compile `web` without the embed.
- **Dev mode.** `vite.config.ts` sets `server.proxy` for `/api`, `/webhooks`, `/healthz` →
  `http://127.0.0.1:8080`. `npm run dev` (Vite on `:5173`) talks to a locally running
  `bin/insteadly`. Production has no separate Node process — the Go binary serves `dist/`.
- **JSON API contract.** The SPA consumes the existing **snake_case `domain.Order` JSON
  verbatim** (no second DTO layer). `web/src/types.ts` mirrors `Order`/`Clinic`/`Patient`/
  `Outcome`/`BookedSlot`/`RecordingFlags`; timestamps are RFC 3339. §5 is the exact surface.

### 3.2 Auth (real, replaces the C5 placeholder)

**Decision: opaque session tokens in a `users`/`sessions` store, `HttpOnly`/`SameSite=Lax`
cookies, bcrypt password hashing, owner-scoped order routes. Auth is opt-in via
`auth.mode` (default `off`) so the C5 no-env smoke is preserved.**

- **Opaque session tokens over JWT.** A session is a 256-bit random token; the server stores
  only `sha256(token)` and looks up by hash. Rationale vs JWT: (a) sessions are trivially
  revocable (logout / password change deletes the row — JWT needs a denylist or short TTL),
  (b) no signature-secret distribution/rotation, (c) the backend already has a durable store
  (Postgres/local) for orders, so a sessions table adds no new infrastructure, and (d) the
  cookie holds a short opaque value, not a base64 blob. JWT's statelessness is a benefit we
  do not use in a single-process prototype.
- **Cookies.** `Set-Cookie: <name>=<raw token>; HttpOnly; SameSite=Lax; Path=/;
  Max-Age=<ttl>; Secure` — `Secure` only when `INSTEADLY_HTTP_PUBLIC_URL` is `https:` (derived
  in the HTTP layer, not a config flag), so dev over `http://127.0.0.1` works. HttpOnly stops
  XSS token theft; SameSite=Lax blocks cross-site POSTs from unrelated origins; JSON-only
  bodies (`Content-Type: application/json`) further raise the CSRF bar. A dedicated CSRF
  token is noted as C7 hardening, not C6.
- **Password hashing: bcrypt (`golang.org/x/crypto/bcrypt`).** Stdlib has none. bcrypt is
  chosen over argon2/scrypt because (a) `x/crypto` already supplies a maintained,
  constant-time `GenerateFromPassword`/`CompareHashAndPassword` API with no manual
  salt/params encoding to get wrong, and (b) `golang.org/x/crypto` is **already an indirect
  dependency** (`v0.58.0`, via pgx), so promoting it to direct costs zero new transitive
  downloads. **Crucially, bcrypt must NOT live in `internal/core`** (dependency law): hashing
  is injected as the `PasswordHasher` port (§4.3), and the bcrypt implementation sits in
  `internal/adapters/password/bcrypt`. The core `auth` service uses only stdlib
  (`crypto/rand`, `crypto/sha256`, `encoding/hex`).
- **Ownership.** `orders.user_id` is set from the session on create; `list/get/events/
  checkout` are scoped to the session user. Cross-user access returns **404** (not 403) so a
  caller cannot distinguish "no such order" from "not yours" (matches the existing
  malformed-ID-is-404 convention).
- **How `INSTEADLY_API_KEY` relates: kept, for machine access only.** `POST
  /api/v1/orders/{id}/call` (the C5 eligibility endpoint) and its in-memory idempotency map
  are **unchanged** and remain the ops/dev "make eligible now" path gated by
  `INSTEADLY_API_KEY`. It is explicitly *not* the user payment path; it stays a documented
  dev/ops override, exactly as C5 shipped it. Real user auth is the new cookie path. This
  preserves every C5 test and the no-env smoke without a second `created→paid` rewrite of the
  machine endpoint.
- **Auth degradation when unconfigured (`auth.mode=off`, the default).** With `off`, order
  routes behave exactly as today (unauthenticated, `user_id` NULL), and
  `register/login/logout/me/checkout` return `501 not_implemented` ("set
  `INSTEADLY_AUTH_MODE=cookie`"). With `cookie`, order routes require a session and scope by
  user. This keeps `make verify` and the C5 no-env smoke (`POST /api/v1/orders` → 201) green
  while making the full web flow available behind one env var. (The alternative — default
  `cookie` and rewrite the smoke — is cleaner product-wise but breaks the binding "do not
  break no-env boot" constraint; flagged for sanity-check in §13.)

### 3.3 Payments (Stripe)

**Decision: Stripe Checkout Sessions + signed webhook; `stripe-go/v87` SDK; `stub` adapter
for dev/no-env; durable idempotency via the state machine + a unique `payment_reference`.**

- **Checkout Session, not PaymentIntent.** Checkout is Stripe's hosted payment page and
  gives us redirect-based checkout (`CreateCheckout → URL → user pays on Stripe → redirect
  back`) with no card-handling code and no PCI surface. PaymentIntent would mean building a
  payment element ourselves. For a one-time fixed-price payment, Checkout is the minimal
  correct choice and matches the `Payments.CreateCheckout(order) → redirectURL` port exactly.
- **Webhook event:** `checkout.session.completed` → `domain.PaymentEvent{OrderID, Paid:true}`.
  The session is correlated to the order via `metadata.order_id` (and `client_reference_id`,
  both set in `CreateCheckout`). The webhook is **always signed and verified** with
  `webhook.ConstructEvent(payload, "Stripe-Signature", whsec)` (HMAC-SHA256 over the
  timestamp + payload); a bad/missing signature → `400`, never processed. Stripe redelivers
  webhooks, so the handler is idempotent (§7).
- **SDK:** `github.com/stripe/stripe-go/v87` (pinned `v87.1.0`, latest major at planning).
  Justified over raw HTTPS because signature verification is subtle and the SDK ships
  `webhook.ConstructEvent` + `GenerateTestSignedPayload` (used by the fake server) for free;
  hand-rolling the `v1` HMAC scheme is exactly the kind of thing that breaks silently. The
  SDK lives entirely inside `internal/adapters/payments/stripe`; the core never sees it.
- **Idempotency — "durable now?".** The C5 in-memory map stays for `POST /call` (unchanged).
  For payments we do **not** build a separate idempotency table. Two layers make a replayed
  `checkout.session.completed` safe: (1) `Order.MarkPaid` is a no-op unless the order is
  `created`, and (2) `orders.payment_reference` (the Stripe checkout session id) is stored
  with a `UNIQUE` partial index, so one payment session can mark at most one order paid.
  This is durable and simpler than a generic key→result table. A cross-provider
  payment-events table is a C7 seam only if a second provider appears.
- **`stub` adapter (`payments.provider=stub`, default).** Keeps no-env boot green and makes
  the whole scheduler flow testable without Stripe: `CreateCheckout` returns a dev URL
  `/api/v1/orders/{id}/checkout/simulate`, and that authenticated GET drives the same
  `MarkPaid` the webhook does. `stripe` is selected by `INSTEADLY_PAYMENTS_PROVIDER=stripe`
  + the two `INSTEADLY_STRIPE_*` secrets.

### 3.4 New dependencies (plan-level justification, exact pins)

| Module / package | Version (pinned) | Justification |
|---|---|---|
| `github.com/stripe/stripe-go/v87` | `v87.1.0` (latest major at planning, verified via `proxy.golang.org/github.com/stripe/stripe-go/v87/@latest` → `v87.1.0`, published 2026-10-29) | Stripe Checkout + signed webhook. Adapter-only; core never imports it. Pulls a moderate transitive set (Stripe's JSON/params helpers); flag any surprise in `go.sum` at `go get` time. |
| `golang.org/x/crypto` | `v0.58.0` (already resolved as an **indirect** dep via pgx; promote to direct) | `bcrypt` password hashing. Zero new transitive downloads because the version is already in the graph. (Latest is `v0.59.0`; keeping `v0.58.0` minimizes churn — bump only if needed.) |

Frontend (npm, `package-lock.json`; versions verified via `npm view <pkg> version` on
2026-11-17):

| Package | Version | Purpose |
|---|---|---|
| `react`, `react-dom` | `^19.3.1` | UI runtime |
| `typescript` | `~7.0.2` (fallback `~5.9.x` if the TS7 native compiler is incompatible with `@vitejs/plugin-react`) | type-check |
| `vite` | `^8.3.8` | dev server + build |
| `@vitejs/plugin-react` | `^6.3.0` | React fast-refresh/JSX |
| `vitest` | `^5.1.0` | unit/component tests |
| `@testing-library/react`, `@testing-library/jest-dom`, `jsdom` | `^16.3.3` / `^7.0.1` / `^31.0.1` | component tests |
| `@playwright/test` | `^1.65.1` (optional, gated) | thin e2e; **not** part of `make verify` |

`go.sum` impact: `stripe-go/v87` adds ~30–60 lines (direct + transitives). `x/crypto` adds
nothing new (already present). `package-lock.json` is new and committed; `web/node_modules/`
and `web/dist/` are gitignored. **No ORM, no second payments provider, no state-management
library, no client router.**

---

## 4. Ports → components and exact contracts

Dependency law (enforced by `make check-deps`): `internal/core/auth` imports only
`internal/core/{ports,domain}` + stdlib. bcrypt and Stripe are adapter-only. `config` stays
stdlib-only (it only trims/checks the Stripe secrets; the adapter parses/uses them).

### 4.1 Domain additions (verbatim)

New file `internal/core/domain/user.go`:

```go
package domain

import "time"

// UserID identifies a registered account.
type UserID string

// NewUserID returns a fresh random user identifier (32 lowercase hex chars).
func NewUserID() UserID { return UserID(newID()) }

// User is a registered account. PasswordHash is the bcrypt hash and is never serialized.
type User struct {
    ID           UserID    `json:"id"`
    Email        string    `json:"email"` // normalized lowercase
    PasswordHash string    `json:"-"`     // bcrypt string; never marshal/log
    CreatedAt    time.Time `json:"created_at"`
}

// Session is one active login. TokenHash is sha256(hex) of the raw cookie token; the raw
// token is never stored, so a store leak yields no usable sessions.
type Session struct {
    TokenHash string    `json:"-"`
    UserID    UserID    `json:"user_id"`
    ExpiresAt time.Time `json:"expires_at"`
    CreatedAt time.Time `json:"created_at"`
}
```

Additions to `internal/core/domain/order.go` (additive, `omitempty` — old JSON still
decodes, the spine table is untouched):

```go
// Order gains (in addition to the existing fields):
UserID           UserID     `json:"user_id,omitempty"`           // owner; nil/zero when auth off or machine-created
PaidAt           *time.Time `json:"paid_at,omitempty"`           // when payment completed (C6)
PaymentReference *string    `json:"payment_reference,omitempty"` // opaque provider id (Stripe checkout session id); unique per order

// MarkPaid records a completed payment and moves created → paid. It is idempotent: an
// order already past "created" is returned unchanged, so a replayed webhook is a no-op.
// reference is the provider's opaque id (Stripe checkout session id), stored for durable
// idempotency; at is the payment time (zero ⇒ now).
func (o *Order) MarkPaid(reference string, at time.Time) error {
    if o.Status != OrderCreated {
        return nil // idempotent: already processed
    }
    if err := o.Transition(OrderPaid); err != nil {
        return err
    }
    t := at
    if t.IsZero() {
        t = time.Now()
    }
    o.PaidAt = &t
    ref := strings.TrimSpace(reference)
    o.PaymentReference = &ref
    return nil
}
```

`MarkPaid` is the seam the payment webhook and the stub "simulate" path both call; it is
the C6 replacement for the C5 manual `created→paid` inside `POST /call` (which keeps using
`Transition(paid)` unchanged). It needs `strings` imported in `order.go`.

### 4.2 `ports.AuthStore` and `ports.PasswordHasher` (verbatim, new file `ports/auth.go`)

```go
package ports

import (
    "context"

    "github.com/artemidius/insteadly/internal/core/domain"
)

// AuthStore persists users and login sessions (C6). Implementations: storage/local (JSON
// users + in-memory sessions) and storage/postgres (users/sessions tables). Passwords are
// NOT handled here — hashing is the PasswordHasher port's job.
type AuthStore interface {
    // CreateUser stores a new user with an already-hashed password. Returns ErrConflict
    // when the email (normalized lowercase) is taken.
    CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error)

    // GetUserByEmail returns the user or ErrNotFound. Email comparison is
    // case-insensitive (the store normalizes to lowercase).
    GetUserByEmail(ctx context.Context, email string) (domain.User, error)

    // GetUser returns the user by ID or ErrNotFound.
    GetUser(ctx context.Context, id domain.UserID) (domain.User, error)

    // CreateSession stores a session keyed by tokenHash (sha256 hex of the raw token).
    CreateSession(ctx context.Context, s domain.Session) error

    // GetSession returns the session by tokenHash, or ErrNotFound when missing or expired.
    GetSession(ctx context.Context, tokenHash string) (domain.Session, error)

    // DeleteSession revokes a session. Idempotent: deleting a missing session is not an error.
    DeleteSession(ctx context.Context, tokenHash string) error
}

// PasswordHasher hashes and verifies passwords. bcrypt adapter in C6; the core auth
// service depends on this port, never on a specific hash algorithm.
type PasswordHasher interface {
    // Hash returns a salted, verifiable hash of password. Never returns the plaintext.
    Hash(ctx context.Context, password string) (string, error)

    // Verify reports whether password matches hash, in constant time where the algorithm
    // allows. A malformed hash must return false, not an error.
    Verify(ctx context.Context, password, hash string) bool
}
```

### 4.3 `internal/core/auth` service (core-only)

```go
// core/auth/service.go — new package internal/core/auth
package auth

// Service is the provider-neutral auth logic: register, login, logout, and cookie-session
// validation. It imports only core/{ports,domain} + stdlib (crypto/rand, crypto/sha256,
// encoding/hex, encoding/base64, errors, strings, time).
type Service struct {
    store  ports.AuthStore
    hasher ports.PasswordHasher
    ttl    time.Duration
    now    func() time.Time // injectable for tests
}

func New(store ports.AuthStore, hasher ports.PasswordHasher, ttl time.Duration) *Service

// Register validates email/password, hashes the password, stores the user, and opens a
// session. Returns the user and the raw session token (caller sets the cookie).
func (s *Service) Register(ctx context.Context, email, password string) (domain.User, string, error)

// Login verifies credentials and opens a session. Returns the user and the raw token.
// Bad email or password both return ErrBadCredentials (no existence oracle).
func (s *Service) Login(ctx context.Context, email, password string) (domain.User, string, error)

// Logout revokes the session for a raw token. Idempotent.
func (s *Service) Logout(ctx context.Context, rawToken string) error

// Authenticate resolves a raw cookie token to a user. Returns ErrUnauthenticated when the
// token is empty, unknown, or expired.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (domain.User, error)

// TokenHash exposes the sha256(hex) of a raw token so tests can assert the at-rest form.
func TokenHash(rawToken string) string
```

- Token generation: 32 bytes from `crypto/rand`, `base64.RawURLEncoding` (43 chars) for the
  cookie value; the store key is `sha256(raw)` hex. Session TTL from config; expiry checked
  on `GetSession`/`Authenticate`.
- Errors: `var ErrBadCredentials = errors.New("bad credentials")`,
  `var ErrUnauthenticated = errors.New("unauthenticated")` — the HTTP layer maps these to
  `401` with a generic message (never "email not found").

### 4.4 `internal/adapters/password/bcrypt`

`bcrypt.New(cost int) ports.PasswordHasher`. `Hash` = `bcrypt.GenerateFromPassword([]byte(p),
cost)` → string; `Verify` = `bcrypt.CompareHashAndPassword` (treat `ErrMismatchedHashAndPassword`
as `false`, any other parse error as `false`). Cost validated `[4,31]` in config; default 12.

### 4.5 Storage schema (DDL, migration `0002_users.sql`)

Add to `internal/adapters/storage/postgres/migrations/`:

```sql
-- 0002_users.sql — C6 users/sessions + order ownership/payment columns.
CREATE TABLE IF NOT EXISTS users (
    id            text PRIMARY KEY,        -- domain.UserID (32 lowercase hex)
    email         text NOT NULL UNIQUE,    -- normalized lowercase
    password_hash text NOT NULL,           -- bcrypt string (never raw)
    created_at    timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash  text PRIMARY KEY,          -- sha256 hex of the raw cookie token
    user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS user_id            text REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS paid_at            timestamptz,
    ADD COLUMN IF NOT EXISTS payment_reference  text;
CREATE INDEX IF NOT EXISTS orders_user_idx
    ON orders (user_id, created_at DESC) WHERE user_id IS NOT NULL;
-- One payment session may mark at most one order paid (durable webhook idempotency).
CREATE UNIQUE INDEX IF NOT EXISTS orders_payment_reference_uniq
    ON orders (payment_reference) WHERE payment_reference IS NOT NULL;
```

The existing migration runner (`postgres/migrate.go`) already applies embedded `.sql` files
in version order under `schema_migrations`; `0002` runs after `0001` on both fresh and
already-migrated DBs. Existing orders get `user_id = NULL` (no FK violation).

Postgres `AuthStore` (`postgres/auth.go`): `CreateUser` → `INSERT … ON CONFLICT (email) DO
NOTHING` (0 rows → `ports.ErrConflict`); `GetUserByEmail` → `SELECT … WHERE lower(email) =
lower($1)`; `GetUser`; `CreateSession`; `GetSession` → `SELECT … WHERE token_hash = $1 AND
expires_at > now()` (`pgx.ErrNoRows` → `ports.ErrNotFound`); `DeleteSession`. `domain.User`
row mapping is trivial (three text/timestamptz columns).

`orders` row/encode changes (`postgres/row.go`): add `user_id text` (NULL-able),
`paid_at timestamptz` (NULL-able), `payment_reference text` (NULL-able) to `orderColumns`,
`orderRow`, `scanOrder`, and `encodeOrder`; `user_id` and `payment_reference` map to
`*string`/`*time.Time` → `domain.Order.UserID`/`*string`/`*time.Time`. `Create`/`Update`
`INSERT`/`UPDATE` column lists gain the three columns.

### 4.6 Local adapter `AuthStore` (parity, dev)

`local.Store` implements `ports.AuthStore` (new file `local/auth.go`):

- **Users persisted** in `<dir>/auth/users.json` (one JSON object keyed by email; values are
  `domain.User` with `password_hash`; written via the existing `writeAtomic`). A single
  `RWMutex` (or a dedicated `authMu`) guards it.
- **Sessions in-memory** (`map[string]domain.Session` keyed by token_hash), mirroring the
  existing in-memory scheduler lease map. Restart invalidates sessions (documented; fine for
  dev). `GetSession` enforces `ExpiresAt > now`.
- `orders.user_id`/`paid_at`/`payment_reference` persist automatically through the existing
  order JSON round-trip (`omitempty`), no `local/orders.go` change beyond the fields being on
  `domain.Order`.

### 4.7 `ports.Payments` (already declared — unchanged)

```go
// ports/payments.go (unchanged from C0)
type Payments interface {
    CreateCheckout(ctx context.Context, order domain.Order) (redirectURL string, err error)
    HandleWebhook(ctx context.Context, payload []byte, signature string) (domain.PaymentEvent, error)
}
```

`domain.PaymentEvent` (unchanged): `{OrderID, Paid bool, Refunded bool, At time.Time}`.
C6 acts on `Paid==true` only; `Refunded` is reserved for C7.

### 4.8 Composition (`internal/app`)

- `App.Payments` becomes **non-nil** (stub or stripe) — fill the existing field.
- `App.Auth *auth.Service` (new field) — constructed from `AuthStore` + `PasswordHasher`.
- `buildStorage` returns the `AuthStore` as an additional value (matching how it already
  returns `SchedulerStore`): `func buildStorage(ctx, c, log) (ports.Storage,
  ports.SchedulerStore, ports.AuthStore, io.Closer, error)`. Local → `store` (it now
  implements `AuthStore`); Postgres → `store` (the pg Store). The composite blob/order split
  is unchanged.
- New builders:
  ```go
  func buildPasswordHasher(c config.Auth, log *slog.Logger) (ports.PasswordHasher, error)
  func buildPayments(c config.Payments, publicURL string, log *slog.Logger) (ports.Payments, error)
  ```
  `buildPayments`: `"stub"` → `paymentsstub.New(log)`; `"stripe"` →
  `paymentsstripe.New(paymentsstripe.Config{SecretKey, WebhookSecret, PriceID, SuccessURL,
  CancelURL, BaseURL(empty=default), Logger})`. Stripe keys are **parsed/used in the
  adapter**; `config` only trims + non-empty-checks them (keeps `config` dependency-free).
- `App.Close`/`CloseStorage` unchanged; the stripe/stub adapters hold no resources to close
  (stripe-go's HTTP client is process-global).

---

## 5. JSON API contract (what the SPA consumes)

Base `/api/v1`, `snake_case`, RFC 3339 timestamps, one error envelope
`{"error":{"code","message"}}` (unchanged). New auth/checkout surface:

| Method/Path | Auth (`cookie` mode) | Request | Response |
|---|---|---|---|
| `POST /api/v1/auth/register` | none | `{"email","password"}` | `201 {"user":{"id","email","created_at"}}` + `Set-Cookie` session |
| `POST /api/v1/auth/login` | none | `{"email","password"}` | `200 {"user":{…}}` + `Set-Cookie` |
| `POST /api/v1/auth/logout` | session | — | `204` + cookie cleared + session revoked |
| `GET /api/v1/auth/me` | session | — | `200 {"user":{…}}` (bootstraps SPA session) |
| `POST /api/v1/orders` | session | existing order body (§5.1) | `201` order with `user_id` set |
| `GET /api/v1/orders` | session | — | `200 [orders]` scoped to the user |
| `GET /api/v1/orders/{id}` | session (owner) | — | `200 order` (non-owner → `404`) |
| `GET /api/v1/orders/{id}/events` | session (owner) | SSE | snapshot + live `order` events |
| `POST /api/v1/orders/{id}/checkout` | session (owner) | — | `200 {"redirect_url":"…"}` |
| `POST /api/v1/orders/{id}/call` | `Bearer <INSTEADLY_API_KEY>` (unchanged) | — | `202` (dev/ops eligibility, C5) |
| `GET /api/v1/orders/{id}/checkout/simulate` | session (owner), stub mode only | — | `303` → dashboard; drives `MarkPaid` |
| `POST /webhooks/payments/stripe` | none (Stripe signature) | raw JSON | `200` (verified) / `400` (bad signature) |

Error codes added (reusing the existing envelope): `401 unauthorized` (missing/expired
session), `404 not_found` (non-owned or missing order — deliberately identical), `409
conflict` (email already registered), `501 not_implemented` (auth off / payments not
configured). Register/login validation: email non-empty + `@` shape, password ≥ 8 chars —
reported in one `400 bad_request`.

### 5.1 Order create body (unchanged, now authenticated)

Same `domain.Order` JSON as [docs/api.md](../docs/api.md): `clinic{name,phone}`,
`patient{full_name,phone,birth_date?,insurance_number?,reason,preferred_slots[]}`,
`recording?`. `id`, `status`, `user_id`, timestamps are server-assigned. The SPA maps the
order form's clinic + patient + preferred slots straight into this body (no transformation).

### 5.2 Checkout response

`POST /{id}/checkout` returns `{"redirect_url":"…"}`. In `stripe` mode that is the Checkout
Session URL; in `stub` mode it is `/api/v1/orders/{id}/checkout/simulate`. The SPA
navigates to it (`window.location.assign`). `CreateCheckout` sets `success_url =
<public_url>/dashboard?order=<id>` and `cancel_url = <public_url>/orders` (or the SPA root in
dev), so Stripe returns the user to the dashboard after payment.

---

## 6. Auth specification

- **Register:** normalize email (trim + lowercase), validate shape, require password ≥ 8.
  `hasher.Hash` → `store.CreateUser`. On `ErrConflict` → `409 conflict` ("email already
  registered"). On success, `openSession` (create 32-byte token, `CreateSession` with
  `TokenHash = sha256(token)`, `ExpiresAt = now + ttl`), then set the cookie and return the
  user.
- **Login:** `GetUserByEmail` → `hasher.Verify`. Unknown email or bad password → the **same**
  `401 unauthorized` + `ErrBadCredentials` (no existence oracle). Success opens a session.
- **Logout:** read the cookie, `DeleteSession(TokenHash(token))`, clear the cookie
  (`Max-Age=0`).
- **Authenticate (middleware):** read the cookie, `Authenticate` → user; on
  `ErrUnauthenticated` → `401`. In `auth.mode=off`, the middleware is a passthrough and the
  order handlers behave as today.
- **Cookie attributes:** `HttpOnly`, `SameSite=Lax`, `Path=/`, `Max-Age=<ttl>`, `Secure`
  iff `INSTEADLY_HTTP_PUBLIC_URL` starts with `https://`. Cookie name from
  `INSTEADLY_AUTH_COOKIE_NAME`.
- **Ownership scoping:** `create` sets `UserID = session.UserID`; `list` filters
  `o.UserID == session.UserID` (handler-side; list is small — SQL `WHERE user_id=$1` is a C7
  optimization); `get`/`events`/`checkout` return `404` when `o.UserID != session.UserID`.

---

## 7. Payments (Stripe) specification

### 7.1 Stripe adapter (`internal/adapters/payments/stripe`)

```go
type Config struct {
    SecretKey     string        // sk_… (secret; never logged)
    WebhookSecret string        // whsec_… (secret; never logged)
    PriceID       string        // one Stripe Price id for the prototype
    SuccessURL    string        // absolute URL Stripe returns the payer to
    CancelURL     string        // absolute URL on cancel
    BaseURL       string        // test-only override of the API origin (fake server); "" = default
    Logger        *slog.Logger
}
```

- **`CreateCheckout(ctx, order)`:** build `checkout/session` params — `Mode: payment`,
  `SuccessURL`, `CancelURL`, `LineItems: [{Price: PriceID, Quantity: 1}]`,
  `ClientReferenceID: string(order.ID)`, `Metadata: {"order_id": string(order.ID)}` — create
  it via the v87 client, return `session.URL`. (The fake server replaces the API origin in
  tests, so this is exercised without real Stripe.)
- **`HandleWebhook(ctx, payload, signature)`:** `webhook.ConstructEvent(payload, signature,
  WebhookSecret)` → on error return `(PaymentEvent{}, err)` (the HTTP layer maps it to 400).
  On `stripe.EventTypeCheckoutSessionCompleted`, unmarshal the session, read
  `Metadata["order_id"]` and the session id, return
  `PaymentEvent{OrderID, Paid: true, At: time.Now()}`. Any other event type returns a
  zero-value `PaymentEvent` (ignored). Signature/API-version tolerance: use
  `ConstructEventWithOptions(…, {IgnoreAPIVersionMismatch: true})` so a Stripe account on a
  different API version does not brick the webhook (documented decision).
- **No live Stripe in tests:** the adapter is constructed with `BaseURL` pointed at an
  in-repo `httptest.Server` (§11). The webhook test signs a synthetic
  `checkout.session.completed` payload with `webhook.GenerateTestSignedPayload`.

### 7.2 Stub adapter (`internal/adapters/payments/stub`)

- `CreateCheckout` → `"/api/v1/orders/" + order.ID + "/checkout/simulate"`.
- `HandleWebhook` → parses a trivial `{"order_id":"…","paid":true}` payload (for tests);
  returns the corresponding `PaymentEvent`.
- No webhook route is mounted for stub; the "payment" is the authenticated
  `GET /{id}/checkout/simulate` handler, which performs `OrderStore.Update(id, func(o){ return
  o.MarkPaid("<stub>/"+id, time.Now()) })` and redirects to the dashboard. This is the exact
  `MarkPaid` seam the Stripe webhook uses, so the scheduler integration path is identical.

### 7.3 Webhook → `paid` bridge (HTTP layer)

`POST /webhooks/payments/stripe` (mounted only when `payments.provider=stripe`):

1. Read the raw body (64 KiB cap) and `Stripe-Signature`.
2. `a.app.Payments.HandleWebhook(ctx, body, sig)`. On error → `400 bad_request` ("invalid
   webhook signature"), never `5xx` (Stripe retries on non-2xx; a bad signature is not
   retryable).
3. If `event.Paid` and `event.OrderID != ""`: `OrderStore.Update(orderID, func(o){ return
   o.MarkPaid(ref, event.At) })` where `ref` is the session id carried in the event (or the
   order's id when absent). `ErrNotFound` (webhook for an unknown order) → `200` with a log
   line, not an error (idempotent ack). After `paid`, the C5 scheduler already lists `paid`
   orders in `ListEligible` and schedules them — **no new dial path is added**.
4. Respond `200`.

`checkout.session.completed` is the only event that transitions an order; duplicates are
no-ops via `MarkPaid` + the `payment_reference` unique index.

---

## 8. API surface — what happens to the existing unauthenticated order endpoints

- **`POST /api/v1/orders`, `GET /api/v1/orders`, `GET /api/v1/orders/{id}`,
  `GET /api/v1/orders/{id}/events`:** unchanged handlers, now wrapped by the session
  middleware. In `auth.mode=off` (default) they behave exactly as C5 (unauthenticated). In
  `cookie` mode they require a session and scope by `user_id` (§6).
- **`POST /api/v1/orders/{id}/call`:** **unchanged** — still API-key-gated, still the dev/ops
  eligibility endpoint, still uses `Transition(paid)` + `Schedule(now)`. It is deliberately
  NOT the user payment path.
- **SSE (`GET /{id}/events`):** unchanged framing; the handler additionally checks ownership
  before subscribing. `GET /api/v1/orders/events` (all-orders SSE) remains a C7 seam, not
  built.
- **SPA fallback:** `mux.Handle("/", web.Handler())` registered **last** so `/api/*`,
  `/webhooks/*`, `/healthz` always win. The handler serves `dist/` files (via `http.FS` +
  `fs.Sub`) and falls back to `index.html` for client-side routes; under `!webembed` it
  returns the "web app not built" placeholder.

---

## 9. Implementation steps (do them in this order; each has a verify command)

> Use `/Library/Developer/CommandLineTools/usr/bin/make`; sandbox builds use
> `GOCACHE=/tmp/gocache-insteadly-c6 GOMODCACHE=/tmp/gomodcache-insteadly-c6`;
> Postgres-tagged tests use `CGO_ENABLED=0`.

1. **Domain additions** — `domain/user.go` (`UserID`/`NewUserID`/`User`/`Session`) and
   `Order.{UserID,PaidAt,PaymentReference}` + `MarkPaid`.
   - Verify: `go test -count=1 ./internal/core/domain/` + new `MarkPaid` idempotency/
     transition tests.
2. **Ports** — `ports/auth.go` (`AuthStore`, `PasswordHasher`).
   - Verify: `go build ./internal/core/ports/`.
3. **`internal/core/auth`** — `Service` (register/login/logout/authenticate/token hash),
   core-only + stdlib, injectable `now`.
   - Verify: `go test -race -count=1 ./internal/core/auth/` (fake store + fake hasher).
4. **bcrypt hasher** — `internal/adapters/password/bcrypt`.
   - Verify: `go test -count=1 ./internal/adapters/password/bcrypt/`.
5. **Local `AuthStore`** — `local/auth.go` (JSON users + in-memory sessions).
   - Verify: `go test -count=1 ./internal/adapters/storage/local/`.
6. **Postgres migration + `AuthStore`** — `0002_users.sql`, `postgres/auth.go`, and the
   `row.go`/`orders.go` column additions.
   - Verify: `go build ./internal/adapters/storage/postgres/`.
7. **Dependency pinning** — `go get github.com/stripe/stripe-go/v87@v87.1.0`; promote
   `golang.org/x/crypto@v0.58.0` to a direct require; record the `go.mod`/`go.sum` deltas.
   - Verify: `go mod tidy && go build ./...`; `make check-deps` still `OK`.
8. **Stripe + stub payments adapters** — `internal/adapters/payments/{stripe,stub}`.
   - Verify: `go test -count=1 ./internal/adapters/payments/...` (fake Stripe httptest).
9. **Config** — `Auth`/`Payments`/`Stripe` structs, registry entries, validation, secret-safe
   `Summary`.
   - Verify: `go test -count=1 ./internal/core/config/` (defaults/env/validation/redaction);
     confirm no-env still boots (`auth.mode=off`, `payments.provider=stub`).
10. **HTTP API** — session middleware + `register/login/logout/me`, ownership scoping,
    `checkout` + `simulate`, Stripe webhook route, SPA fallback mount.
    - Verify: `go test -race -count=1 ./internal/httpapi/` (auth/cookie/ownership/checkout/
      webhook/SSE).
11. **Composition** — `App.Auth`/`App.Payments`, `buildPasswordHasher`/`buildPayments`,
    `buildStorage` returns `AuthStore`.
    - Verify: `go build ./...`; `go test -race -count=1 ./internal/app/`; no-env boot +
      `auth.mode=cookie` boot both serve `/healthz`.
12. **Frontend scaffold** — `web/` Vite+TS+React app, `api.ts`/`types.ts`, `AuthProvider`,
    Register/Login/OrderForm/Dashboard, Vitest setup, `vite.config.ts` proxy, `embed.go`/
    `serve.go`/`noembed.go`, `Makefile` `web`/`web-build` targets, `.gitignore` additions.
    - Verify: `cd web && npm ci && npm run build && npm test` (typecheck + build + Vitest);
      `CGO_ENABLED=0 go build -tags webembed ./cmd/insteadly` after `npm run build`.
13. **Postgres-gated + app-level integration** — `postgres/auth_test.go` (`//go:build
    postgres`), `internal/app/m3_integration_test.go` (stub payments end-to-end).
    - Verify: `CGO_ENABLED=0 go test -tags postgres -count=1 ./...` with Docker.
14. **Docs + PROGRESS** (implementation session only) — `docs/api.md`, `docs/architecture.md`,
    `README.md` (env table + web quickstart), `PROGRESS.md`.
    - Verify: `make verify` (no Docker, no credentials, no Node needed for the Go build);
      `make test-postgres` green; `make web` green.

**Makefile additions:** `web` (`npm ci && npm run build && npm test`), `web-build` (`npm ci
&& npm run build`), and `build` gains the `webembed` tag + `npm run build` prerequisite.
`verify` stays Go-only (no Node) — the `web` package compiles untagged via the placeholder
handler.

---

## 10. Wiring and lifecycle

- `app.New` constructs: storage (+`AuthStore`), telephony/stt/tts/dialogue (unchanged),
  `buildPasswordHasher` (bcrypt), `buildPayments` (stub|stripe), then
  `auth.New(authStore, hasher, cfg.Auth.SessionTTL)` → `App.Auth`, and
  `Payments: payments` → `App.Payments`.
- `httpapi.New` gains the session middleware and mounts `web.Handler()` at `/`. It reads
  `cfg.Auth.Mode` and `cfg.Payments.Provider` to decide which routes are active.
- **Shutdown is unchanged** (C5 order): scheduler → runner → hub → HTTP → storage pool.
  Stripe/stub/bcrypt hold no resources to close.
- **No-env boot** (`go run ./cmd/insteadly`, no `INSTEADLY_*`): `auth.mode=off`,
  `payments.provider=stub`, `storage=local`, scheduler enabled → healthz 200, unauthenticated
  order create 201 (C5 smoke preserved), SPA placeholder served at `/`.

---

## 11. Test matrix (file → cases)

| File | Cases |
|---|---|
| `internal/core/domain/order_test.go` (extend) | `MarkPaid` from `created` → `paid` + `paid_at` + `payment_reference`; `MarkPaid` on `paid`/`scheduled`/`finished` is a no-op; `MarkPaid` does not change the spine table. |
| `internal/core/domain/user_test.go` | `NewUserID` format; `User`/`Session` JSON excludes `password_hash`/`token_hash`. |
| `internal/core/auth/service_test.go` | register hashes + stores + opens session; duplicate email → `ErrConflict`; login ok / bad email / bad password (same error); logout revokes + idempotent; authenticate resolves token → user; expired session → `ErrUnauthenticated`; token uniqueness + `TokenHash` is sha256 hex. |
| `internal/adapters/password/bcrypt/bcrypt_test.go` | hash→verify roundtrip; wrong password false; malformed hash false (not error); cost <4/>31 rejected. |
| `internal/adapters/storage/local/auth_test.go` | create/get/list-by-email/get-by-id; duplicate email → `ErrConflict`; case-insensitive email; session create/get/delete; expired session → `ErrNotFound`; users JSON survives a store reopen (sessions do not). |
| `internal/adapters/storage/postgres/auth_test.go` (`//go:build postgres`) | users/sessions round-trip; unique email → `ErrConflict`; `ON DELETE CASCADE`; expired session filtered; migration idempotence (0001+0002); `orders.user_id` + `payment_reference` unique (second insert with same ref → error). |
| `internal/adapters/payments/stripe/stripe_test.go` | fake `httptest` Stripe server: `CreateCheckout` returns the fake session URL + sets `metadata.order_id`; `HandleWebhook` on a `GenerateTestSignedPayload` `checkout.session.completed` → `PaymentEvent{Paid}`; tampered/missing signature → error; wrong event type → zero `PaymentEvent`; tolerance/API-version options. |
| `internal/adapters/payments/stub/stub_test.go` | `CreateCheckout` returns the simulate URL; `HandleWebhook` parses a stub payload. |
| `internal/httpapi/auth_test.go` | register 201 + `Set-Cookie` (HttpOnly/SameSite/Path/ttl); duplicate 409; login 200 / 401 (both cases same); logout 204 + cookie cleared; `me` 200 / 401; `auth.mode=off` → 501. |
| `internal/httpapi/orders_test.go` (extend) | cookie mode: create sets `user_id`; list scoped; get/events non-owner → 404; checkout → `{redirect_url}`; simulate GET → `paid` + redirect; webhook route → `MarkPaid` + 200; bad signature 400; duplicate webhook no-op. |
| `internal/app/m3_integration_test.go` | **stub payments** end-to-end (local storage + fake telephony/agent): register → login → create order → simulate checkout (`paid`) → scheduler (fake clock) → booked; SSE observes the status sequence; `auth.mode=off` variant keeps the C5 unauthenticated flow green. |
| `web/src/api.test.ts` | fetch wrapper: JSON decode, error-envelope mapping, cookie credentials. |
| `web/src/auth/*.test.tsx` | register/login form validation + submit calls + session bootstrap (mocked fetch). |
| `web/src/orders/OrderForm.test.tsx` | required-field validation; maps clinic/patient/preferred-slots → POST body. |
| `web/src/orders/OrderList.test.tsx` / `OrderDetail.test.tsx` | render list/status; SSE event stream mocked (snapshot then live events). |
| `web/e2e/` (optional, `@playwright/test`) | register → create order → simulate pay → dashboard, against `bin/insteadly` (`auth.mode=cookie`, stub payments). Gated behind a `make e2e-web` target; **not** in `make verify`. |

Fakes: `internal/core/auth` tests use a `fakeStore` (`AuthStore`) + `fakeHasher`
(`PasswordHasher`) — **no bcrypt, no DB, no Stripe, no Twilio**. Stripe adapter tests use an
in-repo `httptest` Stripe server (checkout + `GenerateTestSignedPayload`). The app-level
integration uses `stub` payments and the existing scripted fake telephony. No live payment,
no live phone, no credentials anywhere.

---

## 12. Definition of done

**C6 code complete** (the milestone exit C6 owns — *not* the DESIGN real-phone exit, which
remains blocked by the Twilio trial, nor Q5 deployment):

- [ ] `make verify` green **without Docker, without credentials, and without Node** for the
  Go build: gofmt/vet/race/`check-deps` (`internal/core` imports no adapter)/`go build ./...`
  (the `web` package compiles untagged via the placeholder).
- [ ] `go test -count=1 ./...` green (default, no `postgres` tag).
- [ ] `make test-postgres` green with Docker: `0002` migration, users/sessions round-trip,
  unique email, cascade, `payment_reference` unique, `orders.user_id`.
- [ ] `make web` green: `npm ci && npm run build && npm test` (Vitest + typecheck + build);
  `go build -tags webembed ./cmd/insteadly` succeeds and embeds the real `dist/`.
- [ ] No-env boot: `storage=local`, `auth.mode=off`, `payments.provider=stub`, scheduler
  enabled — `/healthz` 200 and unauthenticated `POST /api/v1/orders` 201 (C5 smoke preserved).
- [ ] `auth.mode=cookie` boot: register/login/logout/me work with HttpOnly cookies; order
  routes are scoped to the session user; non-owner access is 404.
- [ ] The payment bridge (`MarkPaid`) is the production `created → paid` writer via the signed
  Stripe webhook and the stub simulate path; `Runner.Run` remains the **only** dial path
  (unchanged), and a replayed webhook is a no-op (durable via `payment_reference` + status).
- [ ] `config.Summary()` and startup logs never contain the Stripe keys, the webhook secret,
  or any password/token.
- [ ] The SPA (built bundle) is served by the single Go binary with SPA fallback, and its dev
  proxy reaches the Go API.

**Explicitly NOT C6 done:** any Twilio/live-phone work; real Stripe charges (test mode not
required); notifications; per-clinic hours; River; refunds; DESIGN's real-phone M0/M1 exits.

---

## 13. Risks, decision gates, size estimate, C7 seams, sources

### 13.1 Risks / decision gates

1. **`stripe-go/v87` is the first payments dependency.** Justification in §3.4; if `go mod
   tidy` surfaces an unexpectedly large transitive set, stop and re-evaluate (e.g. a narrower
   Stripe client) rather than silently committing a bloated `go.sum`.
2. **Default `auth.mode=off` vs `cookie`.** `off` preserves the binding no-env/C5 smoke; the
   full web flow needs `INSTEADLY_AUTH_MODE=cookie`. If the conductor prefers the web app to
   "just work" out of the box, the default flips to `cookie` and the C5 unauthenticated smoke
   is updated to register-then-create. Gate: explicit conductor sign-off.
3. **Frontend/Node vs `make verify`.** The `webembed` build tag is the guarantee that Go CI
   needs no Node. Enforce with a review rule: any `//go:embed all:dist` must sit behind
   `//go:build webembed`, and `web/dist`/`web/node_modules` must be gitignored.
4. **TypeScript 7 (native compiler).** `typescript` `latest` is `7.0.2`; if `@vitejs/
   plugin-react`/`vite` are not yet compatible, pin `~5.9.x` (dated amendment). Gate: `make
   web` must be green before the chunk is done.
5. **Cookie security.** `Secure` is derived from `INSTEADLY_HTTP_PUBLIC_URL` (https ⇒
   Secure); `SameSite=Lax` + JSON bodies mitigate CSRF. A dedicated CSRF token and
   password-reset/rate-limiting are C7/production hardening, not C6.
6. **Ownership vs existence oracle.** Non-owned orders answer 404 (not 403) so order IDs are
   not leaked; enforce in review.
7. **bcrypt cost vs latency.** Default cost 12 (~200–300 ms) is intentional; do not lower it
   below 10 in production config.
8. **`payment_reference` uniqueness vs the stub.** The stub path sets a distinct
   `"<stub>/<orderID>"` reference so the unique index never collides across orders.

### 13.2 Size estimate

Backend ~1,800–2,400 LOC (auth core ~400, ports ~80, bcrypt ~60, local auth ~180, postgres
auth+migration ~250, stripe ~280, stub ~80, httpapi auth/checkout/webhook ~450, config ~200,
app wiring ~120) plus ~1,800–2,300 LOC of tests. Frontend ~1,200–1,800 LOC (`web/src`) plus
~600–900 LOC of tests and config. Comparable to C5; the risky parts are the Stripe
signature/idempotency correctness and the frontend↔Go serving split, not volume.

### 13.3 C7 seams (do not build now, but leave room)

- `ports.Notifier` (still nil) for email/SMS confirmations/outcomes.
- `WindowProvider` interface for per-clinic hours (Q3).
- River as the production scheduler replacement.
- SQL `WHERE user_id=$1` (`ListByUser`) when the order list outgrows handler-side filtering.
- A cross-provider payment-events table if a second payments provider appears (refunds).
- CSRF token, password reset, email verification, rate-limiting, multi-role authorization.
- Deployment target (Q5): the single binary + a TLS reverse proxy in front of `/webhooks`.

### 13.4 Sources

Repository sources read directly: `DESIGN.md` (§2.1/§2.2/§2.3/§2.4/§5/§6), `PROGRESS.md`,
`plans/06-orchestration.md` (§12 + amendments), `internal/core/ports/{payments,storage,
notify,errors}.go`, `internal/core/domain/{order,ids,notify}.go`,
`internal/httpapi/{server,orders,calls,sse}.go`, `internal/core/config/{config,env}.go`,
`internal/app/{app,providers}.go`, `cmd/insteadly/main.go`,
`internal/adapters/storage/{local,postgres}/*` (incl. `migrations/0001_orders.sql`,
`migrate.go`, `row.go`), `Makefile`, `go.mod`, `docs/{api,architecture}.md`, `README.md`.

External sources actually fetched while planning (2026-11-17):

1. Go module proxy (`proxy.golang.org`): `stripe-go/v87/@latest` → `v87.1.0`
   (2026-10-29); `stripe-go/v8{4,5,6,7}` `@latest` (to confirm v87 is the newest major);
   `golang.org/x/crypto/@latest` → `v0.59.0` (we keep the already-resolved `v0.58.0`).
   `<https://proxy.golang.org/.../@latest>`.
2. [pkg.go.dev — stripe-go/v84/webhook](https://pkg.go.dev/github.com/stripe/stripe-go/v84/webhook)
   — `ConstructEvent(payload, header, secret)`, `ConstructEventWithOptions`
   (`IgnoreAPIVersionMismatch`), and `GenerateTestSignedPayload` used for the fake server.
3. [pkg.go.dev — stripe-go/v84/checkout/session](https://pkg.go.dev/github.com/stripe/stripe-go/v84/checkout/session)
   — `checkout/session.New` and the v84→`stripe.Client` migration note.
4. npm registry (`npm view <pkg> version`): react/react-dom `19.3.1`, vite `8.3.8`,
   typescript `7.0.2`, @vitejs/plugin-react `6.3.0`, vitest `5.1.0`,
   @testing-library/react `16.3.3`, @testing-library/jest-dom `7.0.1`, jsdom `31.0.1`,
   @playwright/test `1.65.1`.

---

## 14. Amendments

### 2026-11-17 — implementation session (Europe/Vienna)

1. **`domain.PaymentEvent` gains an additive `Reference` field.** §4.7 said "unchanged", but
   §7.3 requires the HTTP bridge to persist the Stripe checkout session id as
   `payment_reference`; the port carries no other channel for it. The field is `string`,
   defaults empty, and the `Payments` interface is unchanged. Reason: durable
   (session-id→order) replay protection. Evidence: stripe adapter/webhook tests assert
   `Reference == "cs_test_123"`; `payment_reference` round-trips in local and Postgres tests.
2. **`bcrypt.New(cost int) (*Hasher, error)`** instead of the sketched `New(cost int)
   ports.PasswordHasher`. An out-of-range cost must fail at composition rather than silently
   clamp; cost `0` still selects the default 12, and `config` validates 4–31. Evidence:
   `bcrypt` cost-rejection tests + `app.buildPasswordHasher` tests.
3. **Local users file uses an adapter-local record.** `domain.User.PasswordHash` is
   deliberately `json:"-"`, so `<dir>/auth/users.json` stores an equivalent
   `{id,email,password_hash,created_at}` record instead of marshalling `domain.User`
   directly. Path, 0600 atomic write, and in-memory sessions are exactly §4.6. Evidence:
   local auth tests assert the persisted hash and reopen behavior.
4. **Health payload gains `auth` and `payments` provider names.** `/healthz` previously
   reported only the C1–C5 providers; the new non-secret names make the selected auth and
   payments modes visible. `TestHealth` still asserts no secret values.
5. **Stripe success/cancel URL wiring.** `buildPayments` derives
   `SuccessURL=<public_url>/dashboard` and `CancelURL=<public_url>/orders`; the adapter
   appends `?order=<id>` to the success URL. `config` therefore requires
   `http.public_url` in stripe mode (Stripe Checkout needs absolute URLs). Config parsing
   stays stdlib-only; the adapter owns Stripe types. Evidence: config payment-validation
   tests + stripe `successURL` tests.
6. **`make build` now needs Node and embeds the SPA; `verify` uses a new `build-go`.**
   The Makefile gained `web`, `web-build`, and `build-go`; `build` runs `npm ci &&
   npm run build` and compiles with `-tags webembed`. `verify`/CI lint/test stay Node-free
   (`build-go`). CI's build job and the release workflow gained `actions/setup-node@v7.1.0`
   and release cross-compiles with `webembed`. Evidence: `make verify` (no Node) and
   `make web` both green; `CGO_ENABLED=0 go build -tags webembed ./cmd/insteadly` succeeds.
7. **SPA fallback is GET-only.** `mux.Handle("GET /", ...)` (rather than methodless `/`)
   keeps Go's ServeMux 405 behavior for a wrong method on `/api/v1/orders`; the fallback
   itself answers `404` for unmatched `/api/` and `/webhooks/` paths, and method-scoped
   `/webhooks/` 404 handlers keep an unmounted webhook a 404. Evidence: existing
   `TestWrongMethod` and `TestTelephonyWebhooksMountedOnlyForTwilio` remain green.
8. **Stripe v87 client construction.** v87 deprecates global `stripe.Key` in favour of
   `stripe.NewClient`; the adapter uses `stripe.NewClient(secretKey)` and, for the test-only
   `BaseURL`, `stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL:..., MaxNetworkRetries:
   Int64(0), EnableTelemetry: Bool(false)})` + `stripe.WithBackends`. This keeps the fake
   httptest Stripe server entirely local and sends no telemetry. API-version mismatches are
   ignored via `ConstructEventWithOptions{IgnoreAPIVersionMismatch: true}` as planned.
9. **TypeScript 7.0.2 needed no fallback.** `tsc -p tsconfig.json --noEmit` and Vite 8.3.8
   built the bundle with the planned pins; `~5.9.x` was not required.
10. **Playwright e2e skipped.** It was optional in §11 and is not part of `make verify`;
    Vitest component/API tests cover the UI instead. No `@playwright/test` dependency was
    added.
11. **`auth.mode=off` also makes checkout/simulate return 501**, matching §3.2 ("register/
    login/logout/me/checkout return 501"); stub mode itself is still the default.
12. **Postgres test helper truncates `users`/`sessions`** in addition to `orders`, and the
    pre-existing migration-count assertion now expects two migrations (`0001` + `0002`).
    Evidence: `go test -tags postgres` green.
13. **No Twilio/live-phone or real Stripe work was done or run.** No `.env` or secret value
    was read or printed; the Stripe tests use a local `httptest` server and a synthetic
    webhook signature.
