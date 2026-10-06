# Architecture

## Components

### 1. Go application

Responsibilities:

- authentication
- authorization
- users
- sessions
- API tokens
- rooms
- room membership
- invitations
- messages
- edits/deletes
- reactions
- mentions
- read receipts
- presence
- WebSockets
- attachment metadata and upload authorization
- push registration
- UnifiedPush notifications
- audit logging
- rate limiting
- OpenAPI HTTP API

### 2. PostgreSQL

System of record for all persistent chat state.

Do not store attachment binaries in PostgreSQL.

Use PostgreSQL transactions for message creation and related state changes.

Use LISTEN/NOTIFY only as a lightweight cross-process event signal if multiple server instances are deployed. Do not use NOTIFY as the durable message bus.

### 3. Object storage

Attachments and images belong in S3-compatible storage.

The database stores metadata and storage keys.

Prefer presigned upload/download URLs so large files do not need to pass through the Go process.

The storage implementation should be replaceable without changing API semantics.

### 4. WebSocket

WebSocket is the realtime path for connected clients.

REST remains authoritative for fetching history, pagination, edits, room state, etc.

WebSocket events should be small, typed, versioned, and idempotently processable where practical.

### 5. UnifiedPush / ntfy

The existing self-hosted ntfy server is the push infrastructure.

Android registers through UnifiedPush and sends the resulting registration information to the chat server.

The chat server sends encrypted Web Push notifications through the registered UnifiedPush endpoint.

Do not make the backend dependent on the ntfy Android application's internal APIs.

Do not implement FCM/APNs.

### 6. Clients

Web and Android are separate applications.

Neither client should contain business rules that belong on the server.

Both clients consume the same OpenAPI and WebSocket contracts.

## Security principles

- Passwords use Argon2id or another modern password hashing scheme.
- Never store plaintext passwords.
- Session/API credentials must be revocable.
- Room authorization is enforced server-side on every relevant operation.
- Private room content must never be returned to unauthorized users.
- Attachment access must be authorized.
- Sanitize/validate rich text.
- Never render arbitrary HTML from users without an explicit sanitization policy.
- Rate-limit authentication, invitations, message sending, uploads, and expensive endpoints.
- Audit security-sensitive administrative operations.
- Use secure HTTP headers.
- TLS is expected to terminate at Caddy or another reverse proxy.
