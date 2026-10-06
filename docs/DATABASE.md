# Database Design

Use UUID/ULID-style stable identifiers. Prefer UUIDv7 if the selected PostgreSQL/Go tooling supports it cleanly.

Core tables:

```text
users
sessions
api_tokens
devices
contacts

rooms
room_members
room_invites

messages
message_revisions
message_receipts

attachments
message_attachments

message_reactions
message_mentions

push_endpoints
notification_preferences

audit_log
```

## users

Suggested fields:

- id
- username
- display_name
- email nullable
- password_hash
- role
- enabled
- avatar_attachment_id nullable (FK attachments)
- preferences JSONB (privacy toggles per ADR-013)
- created_at
- updated_at
- last_seen_at

Username must be unique and normalized.

## contacts

Contact-centric UX for clients (ADR-010). Drives the compose modal and contact sidebar in the web client.

- owner_id
- contact_user_id
- created_at

Unique `(owner_id, contact_user_id)`. Relationships are one-way: a mutual pair is two rows.

## sessions

- id
- user_id
- token_hash
- created_at
- expires_at
- revoked_at
- last_seen_at
- user_agent
- ip_address

Never store a raw session token.

## api_tokens

- id
- user_id
- name
- token_hash
- created_at
- expires_at nullable
- revoked_at nullable
- last_used_at

Show the raw token only once at creation.

## devices

Represents client installations.

- id
- user_id
- platform
- client_name
- client_version
- created_at
- last_seen_at

## rooms

- id
- type
- name nullable for direct rooms
- description
- created_by
- archived_at nullable
- created_at
- updated_at

## room_members

- room_id
- user_id
- role
- joined_at
- muted_until nullable
- last_read_message_id nullable

Primary key should be `(room_id, user_id)`.

## room_invites

- id
- room_id
- inviter_id
- invitee_id
- status
- expires_at
- created_at
- accepted_at nullable

## messages

- id
- room_id
- author_id
- reply_to_message_id nullable
- body
- format
- created_at
- edited_at nullable
- deleted_at nullable

Indexes:

- `(room_id, created_at, id)`
- author
- reply target where useful

Cursor pagination should use `(created_at, id)`.

## message_revisions

Keep edit history if enabled.

- id
- message_id
- editor_id
- body
- format
- created_at

## attachments

- id
- uploader_id
- storage_key
- filename
- mime_type
- size_bytes
- sha256
- width nullable
- height nullable
- created_at
- deleted_at nullable

## message_attachments

- message_id
- attachment_id
- sort_order

## reactions

- message_id
- user_id
- emoji
- created_at

Unique `(message_id, user_id, emoji)`.

## mentions

- message_id
- mentioned_user_id
- created_at

Unique `(message_id, mentioned_user_id)`.

## message_receipts

Per-message delivery and read state (ADR-009). The web client renders per-message sent/delivered/read state in the timeline.

- message_id
- user_id
- delivered_at nullable
- read_at nullable

Primary key should be `(message_id, user_id)`.

`delivered_at` is recorded when the WebSocket delivers to that user; `read_at` on read-cursor advancement. `room_members.last_read_message_id` remains the room-level cursor for unread counts and resync.

## push_endpoints

Store the UnifiedPush/Web Push registration information needed to send notifications.

Exact fields must follow the current UnifiedPush/Web Push registration contract and library selected for the Android client.

Do not invent an ntfy-specific device protocol.

Likely concepts include:

- id
- user_id
- device_id
- endpoint
- public_key
- auth_secret
- created_at
- last_seen_at
- revoked_at

Sensitive push credentials should be encrypted at rest where practical.

## notification_preferences

Allow global and room-specific notification settings.

Avoid excessive preference complexity in v1.
