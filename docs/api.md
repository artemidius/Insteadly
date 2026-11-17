# HTTP API

Base URL in development: `http://127.0.0.1:8080`. All bodies are JSON
(`Content-Type: application/json; charset=utf-8`). Field names are `snake_case`;
timestamps are RFC 3339.

## Conventions

- Every response carries `X-Request-Id`. Send your own to correlate logs; otherwise one
  is generated.
- Success responses return the resource directly (no wrapper).
- Errors use one envelope:

  ```json
  {"error": {"code": "bad_request", "message": "field \"clinic.name\" is required"}}
  ```

  | HTTP | `code` | Meaning |
  |---|---|---|
  | 400 | `bad_request` | malformed JSON, unknown field, validation failure |
  | 404 | `not_found` | no such order (malformed IDs count as missing) |
  | 405 | — | wrong method (stdlib ServeMux default body) |
  | 401 | `unauthorized` | missing/expired session, bad login, or a missing/invalid API key |
  | 404 | `not_found` | no such order, or an order owned by another user (deliberately identical) |
  | 409 | `conflict` / `already_scheduled` | email already registered, order ID already exists, or the order cannot be scheduled again |
  | 413 | `payload_too_large` | request body over the route limit (1 MiB orders, 64 KiB webhooks) |
  | 501 | `not_implemented` | auth is off / payments are not configured |
  | 500 | `internal` | unexpected failure (details only in logs) |

## `GET /healthz`

Liveness plus the wired providers. Never contains secrets.

```json
{
  "status": "ok",
  "version": "dev",
  "uptime_seconds": 12,
  "providers": {
    "telephony": "stub", "stt": "stub", "tts": "stub",
    "dialogue": "stub", "storage": "local",
    "auth": "off", "payments": "stub"
  }
}
```

## Authentication (C6)

User auth is **opt-in**. With the default `INSTEADLY_AUTH_MODE=off`, every order route
behaves exactly as in C5 (unauthenticated, `user_id` empty) and the auth/checkout routes
return `501 not_implemented`. Set `INSTEADLY_AUTH_MODE=cookie` to enable registration,
login, and owner-scoped orders.

| Method/Path | Auth | Request | Response |
|---|---|---|---|
| `POST /api/v1/auth/register` | none | `{"email","password"}` | `201 {"user":{…}}` + `Set-Cookie` |
| `POST /api/v1/auth/login` | none | `{"email","password"}` | `200 {"user":{…}}` + `Set-Cookie` |
| `POST /api/v1/auth/logout` | session | — | `204` + cleared cookie + revoked session |
| `GET /api/v1/auth/me` | session | — | `200 {"user":{…}}` |

- Registration normalizes the email to lowercase and requires an `@`-shaped address and a
  password of 8–72 bytes. All violations are reported in one `400 bad_request`. A taken
  email is `409 conflict`.
- Login failures (unknown email and wrong password alike) are the same
  `401 unauthorized {"error":{"code":"unauthorized","message":"invalid email or password"}}`
  so the endpoint is not an account-existence oracle.
- Sessions are opaque 256-bit tokens. Only `sha256(token)` is stored server-side; the raw
  token is an `HttpOnly`, `SameSite=Lax` cookie (`INSTEADLY_AUTH_COOKIE_NAME`, default
  `insteadly_session`). `Secure` is set iff `INSTEADLY_HTTP_PUBLIC_URL` is `https://`.
  The cookie lifetime is `INSTEADLY_AUTH_SESSION_TTL` (default `24h`).
- In `cookie` mode, `POST /api/v1/orders` sets `user_id` from the session, and
  `GET /orders`, `GET /orders/{id}`, `GET /orders/{id}/events`, and checkout routes are
  scoped to that user. Another user's order answers `404` (never `403`), so order IDs are
  not leaked. `user_id`, `paid_at`, and `payment_reference` are server-assigned and rejected
  in the create body.
- `POST /api/v1/orders/{id}/call` is **unchanged**: it is the `INSTEADLY_API_KEY`-gated
  dev/ops eligibility endpoint, not the user payment path.

```bash
curl -s -c /tmp/insteadly-cookies -X POST http://127.0.0.1:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.test","password":"correct horse battery"}'
curl -s -b /tmp/insteadly-cookies http://127.0.0.1:8080/api/v1/auth/me
```

Local-storage sessions are in memory, so restarting a `local`-storage process logs everyone
out. The Postgres store keeps users and sessions durable.

## Checkout and payments (C6)

`POST /api/v1/orders/{id}/checkout` (session, owner) starts a payment and returns
`{"redirect_url": "…"}`. The SPA navigates to that URL.

- `payments.provider=stub` (the default): the redirect points at
  `/api/v1/orders/{id}/checkout/simulate`, an authenticated `GET` that drives the same
  `Order.MarkPaid` seam a real webhook uses and answers `303` to
  `/dashboard?order=<id>`. No external payment happens.
- `payments.provider=stripe`: the redirect is a Stripe Checkout Session URL
  (`success_url=<public_url>/dashboard?order=<id>`,
  `cancel_url=<public_url>/orders`). Stripe returns the payer to the dashboard.

```bash
curl -s -b /tmp/insteadly-cookies -X POST \
  http://127.0.0.1:8080/api/v1/orders/<id>/checkout
# {"redirect_url":"/api/v1/orders/<id>/checkout/simulate"}
curl -s -b /tmp/insteadly-cookies -L \
  http://127.0.0.1:8080/api/v1/orders/<id>/checkout/simulate
```

### `POST /webhooks/payments/stripe`

Mounted only when `INSTEADLY_PAYMENTS_PROVIDER=stripe`. Stripe posts the raw JSON body and
a `Stripe-Signature` header. The handler verifies the HMAC signature
(`INSTEADLY_STRIPE_WEBHOOK_SECRET`), maps `checkout.session.completed` to
`domain.PaymentEvent{OrderID, Paid:true, Reference:<session id>}`, and marks the owned order
paid (`Order.MarkPaid`).

- Bad/missing signature → `400 bad_request` (never processed, never a 5xx that Stripe would
  retry).
- A valid event for an unknown order is acknowledged with `200` and logged.
- Duplicates are safe: `MarkPaid` only acts on `created`, and `orders.payment_reference`
  has a partial unique index. A replayed `checkout.session.completed` is a no-op.
- API-version mismatches are tolerated on purpose
  (`ConstructEventWithOptions{...IgnoreAPIVersionMismatch: true}`), so a Stripe account on a
  different account API version does not brick the webhook.

The only production `created → paid` writer is this payment bridge (or the stub simulate
path); the C5 scheduler still lists `paid` orders and `Runner.Run` remains the only dial
path.

## `POST /api/v1/orders`

Creates an order in status `created`. The server assigns `id`, `created_at`,
`updated_at`, and forces the initial status.

Request fields (unknown fields are rejected):

```json
{
  "clinic": {"name": "Praxis Test", "phone": "+431234567"},
  "patient": {
    "full_name": "Max Mustermann",
    "phone": "+436641234567",
    "birth_date": "1985-04-12",
    "insurance_number": "AT-1234",
    "reason": "Kontrolle",
    "preferred_slots": [
      {"start": "2026-11-20T09:00:00Z", "end": "2026-11-20T10:00:00Z"}
    ]
  },
  "recording": {"recording": true, "transcript": true, "audio": false}
}
```

Rules:

- `clinic.name`, `clinic.phone`, `patient.full_name` are required (non-blank).
- `patient.birth_date`, when present, must match `YYYY-MM-DD`.
- every `preferred_slots` entry must have `start` before `end`.
- `id` must be empty; `status` must be empty or `"created"` — the API cannot mint paid
  orders. `user_id`, `paid_at`, and `payment_reference` are server-assigned and rejected
  when present. All violations are reported in one 400.

Response: `201 Created`, `Location: /api/v1/orders/<id>`, and the stored order:

```json
{
  "id": "ad354a4459ffdff6870e77e85bd04ac0",
  "status": "created",
  "clinic": {"name": "Praxis Test", "phone": "+431234567"},
  "patient": {"full_name": "Max Mustermann", "phone": "+436641234567",
              "birth_date": "1985-04-12", "insurance_number": "AT-1234",
              "reason": "Kontrolle", "preferred_slots": [...]},
  "outcome": null,
  "recording": {...},
  "created_at": "2026-11-16T23:40:22.109274+01:00",
  "updated_at": "2026-11-16T23:40:22.109274+01:00",
  "attempts": 0
}
```

C6 adds ownership/payment fields, omitted when empty: `user_id` (cookie-auth owner),
`paid_at`, and `payment_reference` (the provider's opaque payment id).

C5 adds the scheduler fields, omitted when empty:

- `next_attempt_at`: when the next dial is due (scheduled orders; absent means due now).
- `attempt_history`: append-only prior attempts (`number`, `outcome`, `started_at`,
  `ended_at`), written when the scheduler requeues a callback or retryable failure.

## `GET /api/v1/orders`

Returns orders, newest first by `created_at`; an empty store returns `[]` (never `null`).
With `auth.mode=cookie` the list contains only the session user's orders. With
`auth.mode=off` it returns every stored order (the C5 behavior).

## `GET /api/v1/orders/{id}`

Returns one order or `404 not_found`. `{id}` is the 32-lowercase-hex identifier
generated at creation. With `auth.mode=cookie`, another user's order is also
`404 not_found` (ownership is not distinguishable from existence).

```bash
curl -s http://127.0.0.1:8080/api/v1/orders/ad354a4459ffdff6870e77e85bd04ac0 | jq .status
# "created"
```

## `POST /api/v1/orders/{id}/call`

**Makes an order eligible to dial now** — it does not dial synchronously. When
`INSTEADLY_API_KEY` is set, send `Authorization: Bearer <key>`; an empty key disables auth
for development. The call drives `created → paid → scheduled`
(`next_attempt_at = now`) and returns `202 Accepted`; the scheduler claims and runs it on
its next tick.

- Optional `Idempotency-Key` header (≤ 128 chars). Reusing the key for the same order
  returns the stored order with `200` and does not schedule again. The cache is in-process
  with `INSTEADLY_API_IDEMPOTENCY_TTL` (default 10m); it is not durable (C6).
- `404 not_found` if the order does not exist.
- `409 already_scheduled` if it is already paid/scheduled; `409 conflict` for any other
  status (dialing/ringing/in_call/finished).
- `401 unauthorized` for a missing/wrong key when auth is configured.

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/orders/<id>/call \
  -H 'Authorization: Bearer <key>' -H 'Idempotency-Key: request-1'
```

## `GET /api/v1/orders/{id}/events` (SSE)

A `text/event-stream` for one order. The first `event: order` frame is a full order
snapshot; subsequent frames are order-status events:

```json
{"order_id":"…","status":"scheduled","outcome":null,"attempts":1,"at":"2026-11-17T20:00:00+01:00"}
```

`retry: 3000` is sent once so clients reconnect after 3 s. A `: ping` comment is sent every
15 s. `Last-Event-ID` is accepted but ignored: every (re)connect re-snapshots, so no replay
buffer exists. If a slow client overflows its buffer, the stream ends with
`event: error` / `{"code":"overrun"}` and the client reconnects/re-snapshots. With
`auth.mode=off` the stream is unauthenticated; with `cookie` it requires the session cookie
and returns `404` for another user's order.

```bash
curl -N -b /tmp/insteadly-cookies http://127.0.0.1:8080/api/v1/orders/<id>/events
```

## Telephony webhooks (Twilio)

These routes are **not** part of the JSON API. They exist only when
`INSTEADLY_TELEPHONY_PROVIDER=twilio` and are called by Twilio with
`application/x-www-form-urlencoded` bodies. Status callbacks carry and require
`X-Twilio-Signature`; the TwiML fetch is not signed by Twilio and is instead guarded by
the unguessable call ID plus matching `AccountSid`/`CallSid` form fields. Form bodies over
64 KiB get `413`.

| Method | Path | Response |
|---|---|---|
| `POST` | `/webhooks/telephony/twilio/voice/{callID}` | `200 text/xml` — `<Connect><Stream>` TwiML, or `<Hangup>` for a finished call |
| `POST` | `/webhooks/telephony/twilio/status/{callID}` | `200` empty — call progress callback (`CallStatus`) |
| `POST` | `/webhooks/telephony/twilio/stream-status/{callID}` | `200` empty — stream lifecycle diagnostics |
| `GET` | `/webhooks/telephony/twilio/stream/{callID}` | `101` WebSocket — Media Streams audio |

`{callID}` is the adapter's 32-hex call identifier, not an order ID. These endpoints are
mounted by the same `http.Server` as the JSON API; see [twilio.md](twilio.md) for the
public-URL/tunnel setup and signature troubleshooting.
