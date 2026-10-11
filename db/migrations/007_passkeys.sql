-- +goose Up
-- Passkeys (WebAuthn). The credential is the go-webauthn Credential as JSON;
-- credential_id is lifted out for lookup and uniqueness.
CREATE TABLE user_passkeys (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id bytea NOT NULL UNIQUE,
    credential    jsonb NOT NULL,
    name          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX user_passkeys_user ON user_passkeys (user_id);

-- Single-use WebAuthn ceremony state (go-webauthn SessionData).
CREATE TABLE webauthn_ceremonies (
    token_hash   bytea PRIMARY KEY,
    kind         text NOT NULL CHECK (kind IN ('register', 'login')),
    user_id      uuid REFERENCES users(id) ON DELETE CASCADE, -- null for discoverable login
    session_data jsonb NOT NULL,
    expires_at   timestamptz NOT NULL
);

-- One-time mobile handoff code, bound to the app's PKCE challenge.
CREATE TABLE mobile_handoffs (
    code_hash      bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_challenge text NOT NULL,
    expires_at     timestamptz NOT NULL
);

-- +goose Down
DROP TABLE mobile_handoffs;
DROP TABLE webauthn_ceremonies;
DROP TABLE user_passkeys;
