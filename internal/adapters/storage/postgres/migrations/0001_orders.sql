-- 0001_orders.sql — C5 order store.
-- The schedule lease columns are adapter-local: they are not part of domain.Order and
-- only coordinate scheduler workers. Claim mutates only these columns, never status.
CREATE TABLE IF NOT EXISTS orders (
    id                text PRIMARY KEY,
    status            text NOT NULL,
    clinic            jsonb NOT NULL,
    patient           jsonb NOT NULL,
    outcome           jsonb,
    recording         jsonb,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    attempts          integer NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz,
    attempt_history   jsonb NOT NULL DEFAULT '[]'::jsonb,
    lease_owner       text,
    lease_until       timestamptz,
    CONSTRAINT orders_status_check CHECK (status IN
      ('created','paid','scheduled','dialing','ringing','in_call','finished'))
);

-- Eligibility index: the scheduler's hot query. Partial over only the two claimable
-- statuses so it stays small as finished orders accumulate.
CREATE INDEX IF NOT EXISTS orders_scheduler_due_idx
    ON orders (next_attempt_at, id)
    WHERE status IN ('paid','scheduled');

CREATE INDEX IF NOT EXISTS orders_created_at_idx ON orders (created_at DESC, id DESC);
