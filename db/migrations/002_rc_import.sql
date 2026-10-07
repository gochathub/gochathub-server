-- +goose Up
-- Sidecar for the Rocket.Chat import: preserves auth-enrollment material
-- (TOTP enrollment + backup-code hashes, email-2FA flag) that gochatserver
-- has no schema for yet. The future 2FA feature consumes these rows at
-- rollout; rows are removed with the user (CASCADE on user delete).
CREATE TABLE migrate_rc_users (
    rcid text PRIMARY KEY,
    user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    username text NOT NULL DEFAULT '',
    services jsonb NOT NULL DEFAULT '{}'::jsonb,
    imported_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE migrate_rc_users;