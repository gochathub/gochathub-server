-- +goose Up
CREATE TYPE user_role AS ENUM ('user', 'moderator', 'admin');
CREATE TYPE room_type AS ENUM ('public', 'private', 'direct', 'group_direct');
CREATE TYPE member_role AS ENUM ('member', 'admin');
CREATE TYPE invite_status AS ENUM ('pending', 'accepted', 'declined', 'revoked');
CREATE TYPE message_format AS ENUM ('markdown');
CREATE TYPE attachment_status AS ENUM ('pending', 'ready', 'failed');
CREATE TYPE notif_mode AS ENUM ('all', 'mentions', 'directs', 'never');

CREATE TABLE users (
    id uuid PRIMARY KEY,
    username text NOT NULL CHECK (username = lower(username) AND username ~ '^[a-z0-9_.-]{1,64}$'),
    display_name text NOT NULL,
    email text UNIQUE CHECK (email = lower(email)),
    password_hash text NOT NULL,
    role user_role NOT NULL DEFAULT 'user',
    enabled boolean NOT NULL DEFAULT true,
    avatar_attachment_id uuid,
    preferences jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- IANA tz name (e.g. "Europe/Berlin"); validity is checked in Go
    -- (time.LoadLocation) because the zone database belongs there.
    timezone text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent text NOT NULL DEFAULT '',
    ip_address inet
);

CREATE TABLE api_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz,
    last_used_at timestamptz
);

CREATE TABLE devices (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform text NOT NULL CHECK (platform IN ('android', 'web')),
    client_name text NOT NULL,
    client_version text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE contacts (
    owner_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, contact_user_id),
    CHECK (owner_id <> contact_user_id)
);

CREATE TABLE attachments (
    id uuid PRIMARY KEY,
    uploader_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    storage_key text NOT NULL UNIQUE,
    filename text NOT NULL,
    mime_type text NOT NULL DEFAULT 'application/octet-stream',
    size_bytes bigint NOT NULL DEFAULT 0,
    sha256 text NOT NULL DEFAULT '',
    width integer,
    height integer,
    status attachment_status NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

ALTER TABLE users
    ADD CONSTRAINT users_avatar_fk
    FOREIGN KEY (avatar_attachment_id) REFERENCES attachments(id) ON DELETE SET NULL;

CREATE TABLE rooms (
    id uuid PRIMARY KEY,
    type room_type NOT NULL,
    name text,
    description text NOT NULL DEFAULT '',
    avatar_attachment_id uuid REFERENCES attachments(id) ON DELETE SET NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    pinned_message_id uuid,
    archived_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE room_members (
    room_id uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role member_role NOT NULL DEFAULT 'member',
    joined_at timestamptz NOT NULL DEFAULT now(),
    muted_until timestamptz,
    last_read_message_id uuid,
    archived boolean NOT NULL DEFAULT false,
    PRIMARY KEY (room_id, user_id)
);

CREATE TABLE room_invites (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    inviter_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    invitee_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status invite_status NOT NULL DEFAULT 'pending',
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    accepted_at timestamptz,
    CHECK (inviter_id <> invitee_id)
);

CREATE UNIQUE INDEX room_invites_open_idx ON room_invites (room_id, invitee_id) WHERE status = 'pending';

CREATE TABLE messages (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    author_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reply_to_message_id uuid REFERENCES messages(id) ON DELETE SET NULL,
    body text NOT NULL,
    format message_format NOT NULL DEFAULT 'markdown',
    created_at timestamptz NOT NULL DEFAULT now(),
    edited_at timestamptz,
    deleted_at timestamptz
);

CREATE INDEX messages_room_order_idx ON messages (room_id, created_at, id);
CREATE INDEX messages_author_idx ON messages (author_id);
CREATE INDEX messages_reply_idx ON messages (reply_to_message_id) WHERE reply_to_message_id IS NOT NULL;

ALTER TABLE rooms
    ADD CONSTRAINT rooms_pinned_fk
    FOREIGN KEY (pinned_message_id) REFERENCES messages(id) ON DELETE SET NULL;

CREATE TABLE message_revisions (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    editor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    body text NOT NULL,
    format message_format NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX message_revisions_message_idx ON message_revisions (message_id, created_at);

CREATE TABLE message_receipts (
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    delivered_at timestamptz,
    read_at timestamptz,
    PRIMARY KEY (message_id, user_id)
);

CREATE INDEX message_receipts_user_idx ON message_receipts (user_id, read_at) WHERE delivered_at IS NULL;
CREATE INDEX message_receipts_msg_read_idx ON message_receipts (message_id) WHERE read_at IS NOT NULL;

CREATE TABLE message_attachments (
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    attachment_id uuid NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
    sort_order integer NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, attachment_id)
);

CREATE TABLE message_reactions (
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id, emoji)
);

CREATE TABLE message_mentions (
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    mentioned_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, mentioned_user_id)
);

CREATE INDEX message_mentions_user_idx ON message_mentions (mentioned_user_id);

CREATE TABLE push_endpoints (
    id uuid PRIMARY KEY,
    device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint text NOT NULL UNIQUE,
    public_key text NOT NULL,
    auth_secret text NOT NULL,
    validation_token_hash bytea,
    validated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE INDEX push_endpoints_user_idx ON push_endpoints (user_id) WHERE revoked_at IS NULL;

CREATE TABLE notification_preferences (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    room_id uuid REFERENCES rooms(id) ON DELETE CASCADE,
    mode notif_mode NOT NULL DEFAULT 'mentions'
);

CREATE UNIQUE INDEX notif_pref_pair_idx ON notification_preferences (user_id, room_id);
CREATE UNIQUE INDEX notif_pref_user_default_idx ON notification_preferences (user_id) WHERE room_id IS NULL;

CREATE TABLE audit_log (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    action text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    ip_address inet,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_created_idx ON audit_log (created_at);
CREATE INDEX audit_log_action_idx ON audit_log (action, created_at);

CREATE TABLE app_config (
    key text PRIMARY KEY,
    value jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP INDEX audit_log_action_idx;
DROP INDEX audit_log_created_idx;
DROP TABLE audit_log;
DROP INDEX notif_pref_user_default_idx;
DROP TABLE notification_preferences;
DROP INDEX push_endpoints_user_idx;
DROP TABLE push_endpoints;
DROP INDEX message_mentions_user_idx;
DROP TABLE message_mentions;
DROP TABLE message_reactions;
DROP TABLE message_attachments;
DROP INDEX message_receipts_msg_read_idx;
DROP INDEX message_receipts_user_idx;
DROP TABLE message_receipts;
DROP INDEX message_revisions_message_idx;
DROP TABLE message_revisions;
DROP INDEX messages_reply_idx;
DROP INDEX messages_author_idx;
DROP INDEX messages_room_order_idx;
DROP TABLE messages;
DROP TABLE room_invites;
DROP TABLE room_members;
DROP TABLE rooms;
ALTER TABLE users DROP CONSTRAINT users_avatar_fk;
DROP TABLE attachments;
DROP TABLE contacts;
DROP TABLE devices;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE app_config;
DROP TABLE users;
DROP TYPE notif_mode;
DROP TYPE attachment_status;
DROP TYPE message_format;
DROP TYPE invite_status;
DROP TYPE member_role;
DROP TYPE room_type;
DROP TYPE user_role;