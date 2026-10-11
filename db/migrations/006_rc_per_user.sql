-- +goose Up
-- Rocket.Chat bridge becomes per-user (ADR-024): each link carries its own
-- workspace URL, and mirrored rooms/messages/shadow users are keyed by the
-- link owner (ids derive from owner + server + remote id), so two local users
-- never share mirrored rows. The old rid-only unique index would forbid that.
DROP INDEX IF EXISTS rooms_source_rid_key;
ALTER TABLE rocket_chat_links ADD COLUMN base_url text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE rocket_chat_links DROP COLUMN IF EXISTS base_url;
CREATE UNIQUE INDEX IF NOT EXISTS rooms_source_rid_key ON rooms (source_rid) WHERE source <> '';
