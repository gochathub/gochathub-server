-- +goose Up
-- TOTP second factor. Secrets are stored as base32 plaintext (same as the
-- Rocket.Chat rows imported below); backup codes are hex SHA-256 of the code.
CREATE TABLE user_totp (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    -- last accepted 30s time step; a code is valid only for a step > this
    last_step bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_backup_codes (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash text NOT NULL,
    used_at timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

-- Step-1 result of a password-verified login for a 2FA user: no session
-- exists until the challenge is redeemed with a valid code.
CREATE TABLE login_challenges (
    token_hash bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    attempts int NOT NULL DEFAULT 0
);

-- Fold in Rocket.Chat enrollments preserved by the rcmigrate sidecar
-- (email-2FA is intentionally dropped: no email flows).
INSERT INTO user_totp (user_id, secret, enabled)
SELECT user_id, services->'totp'->>'secret', true
FROM migrate_rc_users
WHERE user_id IS NOT NULL
  AND services->'totp'->>'secret' ~ '^[A-Z2-7]+$'
  AND COALESCE((services->'totp'->>'enabled')::boolean, false)
ON CONFLICT DO NOTHING;

INSERT INTO user_backup_codes (user_id, code_hash)
SELECT m.user_id, h
FROM migrate_rc_users m, jsonb_array_elements_text(COALESCE(m.services->'totp'->'hashedBackup', '[]'::jsonb)) h
WHERE m.user_id IN (SELECT user_id FROM user_totp)
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE login_challenges;
DROP TABLE user_backup_codes;
DROP TABLE user_totp;
