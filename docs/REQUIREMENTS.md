# Functional Requirements

## Users

- Local user accounts.
- Username.
- Display name.
- Email optional/configurable.
- Password authentication.
- Account enabled/disabled state.
- Roles: user, moderator, admin.
- Session management.
- API tokens for integrations.
- Device registrations.

## Rooms

Room types:

- public
- private
- direct/group-direct

A room has:

- name
- slug or stable identifier
- description/topic
- creator
- created/updated timestamps
- archived state
- membership policy

Public rooms may be discoverable and joinable according to server policy.

Private rooms require membership/invitation.

## Invitations

Support:

- invite user to room
- accept invite
- decline/revoke invite
- expiration
- inviter
- invitee
- room
- status

## Messages

Support:

- rich text/Markdown-style content
- attachments
- images
- replies/thread references
- mentions
- edits
- soft deletion
- reactions
- timestamps
- author
- room
- stable message ID

Messages must be ordered deterministically.

Use cursor pagination, not offset pagination, for message history.

## Rich text

Do not store arbitrary executable HTML.

Define a documented Markdown/rich-text subset.

Server validates content and metadata.

Clients render the same documented subset.

## Attachments

Support:

- arbitrary files
- images
- MIME type
- filename
- byte size
- checksum
- dimensions for images where applicable
- upload lifecycle
- deletion
- authorization

Use direct-to-object-storage upload where practical.

## Read state

Track per-user/per-room:

- last read message
- unread count derivable from message/read state
- optionally last read timestamp

Do not maintain redundant counters unless there is a measured performance reason.

## Presence

Initial implementation can be lightweight:

- online
- away
- offline

Do not build a complex presence subsystem.

Presence should be ephemeral and should not become a core database dependency.

## Notifications

Server determines when a push is appropriate.

At minimum support user/device preferences for:

- all messages
- mentions
- direct messages
- room-specific mute

Do not send a push for every message when the recipient is actively connected to the relevant room unless the user's notification policy says otherwise.

## CLI

Examples:

```text
chat-server user list
chat-server user create <username>
chat-server user disable <username>
chat-server user enable <username>
chat-server user passwd <username>
chat-server user delete <username>

chat-server room list
chat-server room create <name>
chat-server room archive <room>

chat-server room members <room>
chat-server room invite <room> <username>

chat-server token create <username>
chat-server token revoke <token-id>

chat-server migrate
chat-server version
```

CLI operations should use application/service-layer logic rather than duplicating business rules in raw SQL.
