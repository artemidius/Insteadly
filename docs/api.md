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
  | 409 | `conflict` | order ID already exists |
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
