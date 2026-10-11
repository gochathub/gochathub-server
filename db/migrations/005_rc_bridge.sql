-- +goose Up
-- Rocket.Chat live bridge (docs/ROCKETCHAT-INTEGRATION.md). Rooms mirrored
-- from a live Rocket.Chat workspace are real local rooms with a source marker
-- so the regular room/message/push flow serves them untouched.
--
-- source_rid carries the Rocket.Chat room id; the deterministic uuidv5 room
-- id (migrate.NSRoom) also matches rcmigrate-imported rooms, so linking a
-- previously imported workspace reopens the same rooms instead of duplicating.

ALTER TABLE rooms ADD COLUMN source text NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN source_rid text NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN source_synced_at timestamptz;
CREATE UNIQUE INDEX rooms_source_rid_key ON rooms (source_rid) WHERE source <> '';

-- One link per local user: their delegated Rocket.Chat credentials. The PAT
-- is stored AES-GCM-encrypted (key from RC_PAT_KEY env) exactly like other
-- long-lived secrets: never logged, never returned by an API response.
CREATE TABLE rocket_chat_links (
    user_id      uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    rc_user_id   text NOT NULL,
    rc_username  text NOT NULL DEFAULT '',
    pat_enc      bytea NOT NULL,
    watermark    timestamptz,          -- subscriptions.get?updatedSince cursor
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

-- +goose Down
DROP TABLE IF EXISTS rocket_chat_links;
DROP INDEX IF EXISTS rooms_source_rid_key;
ALTER TABLE rooms DROP COLUMN IF EXISTS source;
ALTER TABLE rooms DROP COLUMN IF EXISTS source_rid;