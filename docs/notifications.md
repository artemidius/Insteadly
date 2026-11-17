# Notifications (C7)

C7 gives the `ports.Notifier` port its first adapters and a small core worker that turns a
terminal order outcome into one outbound notification. There is no real email/SMS provider:
the adapters are `log` (no side effect) and `webhook` (one HTTP POST). A durable outbox,
retries, and webhook signing are later seams.

## Configuration

```bash
INSTEADLY_NOTIFY_PROVIDER=off            # default: no notifier, no worker, App.Notify == nil
INSTEADLY_NOTIFY_PROVIDER=log            # structured log only
INSTEADLY_NOTIFY_PROVIDER=webhook
INSTEADLY_NOTIFY_WEBHOOK_URL=https://…   # secret; required for webhook
INSTEADLY_NOTIFY_WEBHOOK_CHANNEL=email   # email | sms (default email)
```

`notify.provider=off` is the zero-config behavior: `App.Notify` stays nil and no worker is
started, so the process behaves exactly as before C7. The webhook URL is a secret and is
redacted from startup logs/`/healthz` (never its value); `/healthz` reports only
`notify=<provider>`.

## Adapters

`internal/adapters/notify/log`

- `New(log *slog.Logger)`.
- `Send` logs the non-secret envelope at `Info`: order id, channel, recipient, subject, and
  the body's **length**. The body is not logged because it can carry patient context.

`internal/adapters/notify/webhook`

- `New(url, channel string, log *slog.Logger)` (plus `NewWithClient` for tests).
- `Send` POSTs `{"order_id","channel","to","subject","body"}` as JSON with a bounded timeout
  (10 s default). A non-2xx response is a retryable error for the caller; the response body
  is never retained or logged. Transport errors are sanitized so the secret URL (including
  its path) never lands in a log line.
- The configured channel is the default; a non-empty `domain.Notification.Channel` (set by
  the worker's recipient resolution) wins.
- No signatures in v1 — a webhook signing secret is a later seam.

Both implement the existing `ports.Notifier` verbatim; there is no port change.

## Worker

`internal/core/notify` is core (imports `domain`, `ports`, `events`, stdlib only):

```go
cfg := notify.Config{
    Events:   hub,              // events.Subscriber (the existing order-status hub)
    Orders:   storage.Orders(), // recipient resolution
    Users:    authStore,        // optional; nil = no email resolution
    Notifier: notifier,
    Logger:   log,
}
worker, err := notify.New(cfg)
go worker.Run(ctx)
```

- **Trigger:** subscribes to the existing `events.Hub` and reacts only to
  `OrderEvent{Status: finished, Outcome != nil}` — the terminal event the runner already
  publishes after `persist`. No runner change was needed.
- **Recipient resolution:** if `order.UserID != ""` and a Users store is configured,
  `GetUser` → `Channel="email"`, `To=user.Email`; else if `order.Patient.Phone` is non-empty
  → `Channel="sms"`, `To=patient.Phone`; else `Channel="email"`, `To=""` (the log notifier
  still records it; the webhook still posts it). A failed user lookup logs and falls back to
  the patient phone.
- **Dedupe:** keyed `(OrderID, Attempts)` with a bounded (2048-entry) FIFO map. A replayed
  terminal event is ignored; a later attempt is a distinct notification.
- **Format:** a fixed, non-secret subject/body from the outcome:
  - `booked` → "Termin bestätigt" + date/doctor/practice;
  - `callback_needed` → "Rückruf erforderlich" + callback time;
  - `failed` → "Anruf fehlgeschlagen" + reason;
  - `aborted` → "Anruf abgebrochen".
  No patient conversation text, payment data, or free-form detail is included.
- **Best-effort:** each `Send` is bounded (15 s); an error is logged and **not** retried
  infinitely. The worker keeps running for later orders.

## Lifecycle

`App.Start` starts the worker (it must subscribe before the scheduler/runner publish the
first terminal event) and then the scheduler, once. `App.Close` stops the scheduler, drains
the runner, then cancels and waits for the notify worker, and only then closes the event
hub, so the worker can still deliver any final finished event. All of this is
idempotent.

`notify.provider=off` builds neither the notifier nor the worker.

## Known limitation

The worker is a normal hub subscriber, and the hub drops a slow subscriber when its bounded
buffer overflows (it closes that subscription). A slow `webhook` endpoint can therefore drop
a `finished` event; the worker then exits its loop as the hub closes the channel. This is
accepted for C7 and is why the `log` notifier is the low-risk choice; a durable outbox with
retry is the next hardening step.
