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
  | 401 | `unauthorized` | missing/invalid API key for a protected endpoint |
  | 409 | `conflict` / `already_scheduled` | order ID already exists, or the order cannot be scheduled again |
  | 413 | `payload_too_large` | request body over 1 MiB |
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
    "dialogue": "stub", "storage": "local"
  }
}
```

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
  orders (payments do that in C6). All violations are reported in one 400.

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

C5 adds the scheduler fields, omitted when empty:

- `next_attempt_at`: when the next dial is due (scheduled orders; absent means due now).
- `attempt_history`: append-only prior attempts (`number`, `outcome`, `started_at`,
  `ended_at`), written when the scheduler requeues a callback or retryable failure.

## `GET /api/v1/orders`

Returns all orders, newest first by `created_at`; an empty store returns `[]` (never
`null`).

## `GET /api/v1/orders/{id}`

Returns one order or `404 not_found`. `{id}` is the 32-lowercase-hex identifier
generated at creation.

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
`event: error` / `{"code":"overrun"}` and the client reconnects/re-snapshots. No auth in
C5 (the whole API is prototype-scoped until C6).

```bash
curl -N http://127.0.0.1:8080/api/v1/orders/<id>/events
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
