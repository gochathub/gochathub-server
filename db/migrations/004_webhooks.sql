-- +goose Up
-- Inbound webhooks (docs/WEBHOOKS.md). Each webhook posts as its own bot user
-- into one fixed room (a user target is a bot<->user DM room).
-- ADD VALUE is allowed inside a transaction on PG12+ as long as the new value
-- is not used in this migration; bot rows are inserted at runtime.
ALTER TYPE user_role ADD VALUE 'bot';

CREATE TABLE webhooks (
    id             uuid PRIMARY KEY,
    name           text NOT NULL,
    bot_user_id    uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE RESTRICT,
    room_id        uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    secret_hash    bytea NOT NULL,
    -- empty = any source address; entries are CIDR strings validated in Go
    allowed_cidrs  text[] NOT NULL DEFAULT '{}',
    -- NULL = spam gate off; compared with the X-Spam-Score header
    max_spam_score real,
    enabled        boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    rotated_at     timestamptz,
    last_used_at   timestamptz
);

-- One row per accepted source message id (Postmark MessageID / Message-ID).
CREATE TABLE webhook_deliveries (
    webhook_id uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    source_id  text NOT NULL,
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, source_id)
);

-- +goose Down
-- The 'bot' enum value cannot be dropped; it is harmless when unused.
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
