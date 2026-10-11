-- +goose Up
-- Email signup invites (ADR-026). Token is stored hashed; status is derived
-- from used_at / revoked_at / expires_at.
CREATE TABLE signup_invites (
    id           uuid PRIMARY KEY,
    email        text NOT NULL CHECK (email = lower(email) AND position('@' in email) > 1),
    token_hash   bytea NOT NULL UNIQUE,
    invited_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_sent_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    revoked_at   timestamptz
);
-- at most one open invite per email
CREATE UNIQUE INDEX signup_invites_open_email
    ON signup_invites (email) WHERE used_at IS NULL AND revoked_at IS NULL;

-- users.username was never constrained unique (001_init has only a CHECK), so
-- the CLI's conflict path could not fire. Public signup needs it. Fails loudly
-- if duplicates already exist: resolve them by hand, never auto-delete.
CREATE UNIQUE INDEX users_username_key ON users (username);

-- +goose Down
DROP INDEX users_username_key;
DROP TABLE signup_invites;
