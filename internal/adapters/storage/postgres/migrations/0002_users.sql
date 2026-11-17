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
