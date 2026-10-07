# WebSocket Protocol

Endpoint:

```text
GET /api/v1/ws
```

Authenticate using the same session/API-token mechanism as REST.

Avoid putting long-lived credentials in query parameters. For browser clients the session cookie (ADR-015) authorizes the upgrade; Android and integrations use API tokens.

## Connection flow

1. HTTP upgrade.
2. Authenticate.
3. Server sends `connected`.
4. Client sends `subscribe` for rooms.
5. Server sends room events.
6. Client sends acknowledgements (recorded as delivered receipts, ADR-009) and read-state updates as appropriate.
7. Heartbeat keeps connection alive.

## Event envelope

Use a stable envelope:

```json
{
  "type": "message.created",
  "id": "event-id",
  "timestamp": "2026-10-06T15:00:00Z",
  "room_id": "room-id",
  "data": {}
}
```

Recommended event types:

```text
connected

room.created
room.updated
room.archived
room.member_added
room.member_removed
room.member_role_changed

message.created
message.updated
message.deleted
message.receipts_changed

message.reaction_added
message.reaction_removed

room.pinned_changed

room.read_state_changed

typing.started
typing.stopped

presence.changed

invite.created
invite.accepted
invite.revoked

contact.added
contact.removed
contact.updated
```

Typing and presence are ephemeral and should not be persisted.

## Reliability

WebSocket is not the authoritative storage mechanism.

Membership revocation must drop live subscriptions: the hub's room index is
a manually-invalidated cache of `room_members`. Every server-side write path
that removes a member MUST call `Notify.RemoveFromRoom(userID, roomID)` —
`RoomService.RemoveMember` is the single chokepoint (admin kick and
self-leave both route through it). Granting membership needs no hub call;
subscribing rechecks the database.

If a connection drops:

1. reconnect
2. fetch missed state/messages using REST
3. resume WebSocket subscription

Do not build a complicated distributed event replay system for v1.

The client should be able to recover from any WebSocket interruption.

## PostgreSQL events

For multiple Go processes:

```text
transaction
  ├── INSERT message
  └── NOTIFY chat_event
```

A listener wakes other instances.

Those instances can query PostgreSQL for authoritative data and deliver WebSocket events.

Do not put the complete message payload into PostgreSQL NOTIFY.
