# Architectural Decisions

## ADR-001: Go application instead of PostgREST

Use Go for business logic.

Reason:

Chat requires authorization, WebSockets, notification orchestration, file handling, sessions, rate limiting, and client-specific realtime behavior. These are application concerns rather than simple database CRUD.

## ADR-002: PostgreSQL is the source of truth

All durable chat state lives in PostgreSQL.

## ADR-003: WebSocket + REST

REST is authoritative and integration-friendly.

WebSocket is a realtime optimization/transport.

Clients recover through REST after disconnects.

## ADR-004: ntfy via UnifiedPush

The existing ntfy infrastructure is used through UnifiedPush rather than creating a custom Android push system.

## ADR-005: No message broker

Do not add Redis/Kafka/RabbitMQ initially.

PostgreSQL transactions plus optional LISTEN/NOTIFY are sufficient for the intended architecture.

## ADR-006: Object storage for files

Do not put binary attachments in PostgreSQL.

## ADR-007: Separate clients

Web and Android are independent clients of the same backend.

## ADR-008: API-first

OpenAPI is a first-class contract because the server must integrate with existing custom applications.

## ADR-009: Per-message receipts

Add `message_receipts` (message_id, user_id, delivered_at, read_at) and report per-message sent/delivered/read state.

Reason: the web client (Avian fork) renders per-message delivery and read states in the message timeline. A room-level read cursor alone cannot produce them. Delivered is recorded on WebSocket delivery acknowledgement, read on read-cursor advancement. This supersedes the `DATABASE.md` guidance that avoided a separate receipt table; the Android client consumes the same states.

## ADR-010: Contacts table

Add a `contacts` table (owner_id, contact_user_id, unique pair) with add/remove/list operations and WebSocket sync events.

Reason: the web client is contact-centric — its compose modal and contact sidebar are driven by a friend list rather than room membership. Adding the entity server-side keeps the Android client's behavior consistent instead of diverging between platforms.

## ADR-011: Avatars via attachment upload with Gravatar fallback

Users and rooms carry an avatar uploaded through the attachment flow, referenced as `avatar_attachment_id`. The server resolves and exposes a `avatar_url` field in user/room payloads; when no upload exists and the user has an email, the URL falls back to Gravatar (derived server-side); with neither, clients render initials.

Reason: the web client renders avatars everywhere. Centralizing resolution server-side avoids leaking raw email addresses to other clients and keeps presigned-URL short-lived behavior consistent with attachments.

## ADR-012: Voice calls excluded from scope

No WebRTC signaling, call tables, or call endpoints. The Avian fork strips the call UI (dialer, call history, call modals).

Reason: calling is a large new subsystem absent from all requirements; if it ever becomes a requirement it will need its own ADR covering signaling transport and TURN/STUN.

## ADR-013: Privacy preferences as JSONB

Store the four client privacy toggles (`last_seen_visible`, `read_receipts`, `allow_group_invites`, `allow_private_messages`) in a JSONB `users.preferences` column, served by `GET/PATCH /users/me/preferences`.

Reason: the settings surface is small, low-stakes, and likely to grow; a JSONB column extends without migrations. Visibility of last-seen and read receipts is enforced server-side when generating payloads.

Defaults are functionality-on, privacy-off: unknown/missing keys mean
`allow_group_invites` and `allow_private_messages` are allowed (chat works),
`last_seen_visible` and `read_receipts` are hidden (opt-in sharing). The
read-receipts preference also removes a user from sender-visible message
receipt aggregation (ADR-009) when set to false.

## ADR-014: Accounts are CLI-managed only

No self-registration, password reset, email verification, or SMTP integration. Users are created and password-reset through the admin CLI (`chat-server user create`, `chat-server user passwd`).

Reason: the deployment is self-hosted with no email infrastructure; account onboarding is an administrator action. The web fork strips the signup and password-reset pages. Revisit with an ADR if public sign-up is ever required.

## ADR-015: Web clients authenticate with an httpOnly session cookie

Browser clients authenticate via a server-side opaque session delivered as a `Set-Cookie` with the `__Host-` prefix, `Secure`, `SameSite=Lax`, `HttpOnly`. The same cookie authorizes the WebSocket upgrade, so credentials never appear in query parameters (`WEBSOCKETS.md`). Bearer API tokens remain the mechanism for Android, CLI, and integration clients.

All state-changing requests authenticated by the session cookie must pass a same-origin check (`Origin`/`Referer` match against the deployment origin) in addition to `SameSite=Lax`, to close the residual CSRF surface.

Login issues the token two ways by request: browser clients (default) get the
cookie only — the response body carries no token; clients that send
`token_request: true` (Android, integrations) receive the same opaque token in
the body for `Authorization: Bearer` use. One session, two transports.

Reason: httpOnly removes the token from XSS-read memory (the Avian template's `localStorage.token` pattern is dropped), and cookie-based browser sessions are the standard revocable mechanism for this API.

## ADR-016: Single origin, no CORS support

The deployment serves the web app and the API behind one origin through the reverse proxy. The API does not implement CORS.

Reason: CORS is deployment surface and attack surface for no requirement — Android and custom integrations do not care about CORS. A cross-origin web deployment can proxy instead. Revisit with a new ADR if a separate web origin is genuinely needed.

## ADR-017: Gravatar fallback parameters

When a user has no uploaded avatar and a stored email, `avatar_url` uses the Gravatar URL form `https://gravatar.com/avatar/<sha256(email)>?s=256&d=404` — SHA-256 of the lowercased trimmed email. The server does not proxy or HEAD-check; the client falls back to monogram rendering on a 404.

Reason: SHA-256 is Gravatar's current published hash; `d=404` lets clients detect absence without a placeholder identity; no server proxy avoids per-request latency and gravatar availability coupling.

Known trade-off: the email hash is exposed to any client that receives the payload, which permits offline correlation for known email addresses. Accepted for this deployment class (admin-provisioned accounts, no public sign-up).

## ADR-018: Hidden last-seen renders as null

When the target user's `last_seen_visible` preference disallows it, `last_seen_at` is serialized as JSON `null` rather than omitted.

Reason: `User` keeps a stable schema for generated clients (`last_seen_at` is a declared nullable field); omitting the field would make every client implementation handle absent-vs-null branching for no benefit.

## ADR-019: Rocket.Chat import — deterministic ids, sidecar schema, legacy bcrypt

`rcmigrate` imports a Rocket.Chat mongodump archive (`internal/migrate`, one-shot/delta). Choices:

- Deterministic UUIDv5 ids derived from source ids (fixed namespaces per kind) — re-runs upsert instead of duplicating; needed for repeatable cutover with fresh backups. No id-map table.
- Delta cursor in `app_config` (`migrate.rc`): message/avatar timestamps; only advances on runs that imported attachments (`--skip-files` sets `files_pending`).
- Legacy password hashes import as `bcrypt$` (bcrypt over SHA-256 hex, the Rocket.Chat/Meteor scheme); `pwd.Verify` accepts them and login re-hashes to argon2id. No plaintext is ever carried.
- 2FA enrollment (TOTP `secret`/`hashedBackup`, `email2fa`) is stored in a dedicated `migrate_rc_users` sidecar table — the 2FA feature does not exist yet, so no speculative schema; the future feature consumes this table at rollout. Resume tokens, cloud credentials and password history are not carried over.
- Archive parsing uses mongo-driver BSON decoding directly against the mongodump stream; no MongoDB instance is required at import time.

Reason: correctness of idempotent re-runs beats inventing a migration-tracking schema; keeping untouched auth material in a sidecar avoids both loss (2FA must re-enroll otherwise) and schema speculation. The sidecar rows are removed with the user (CASCADE) and the table can be dropped once 2FA consumes the data.

## ADR-020: Webhook messages are authored by a bot user; target is fixed per webhook

Inbound webhooks (`docs/WEBHOOKS.md`) post as a dedicated bot user per webhook (`users.role = 'bot'`, unusable password hash, login rejected, no sessions/tokens), and the webhook row binds exactly one room. A user target is a bot-to-user DM room created when the webhook is created. The payload never selects the recipient.

- `messages.author_id` is a user FK and membership is checked against the principal, so a real user row is required; impersonating the creating admin or a shared system user was rejected (provenance, per-webhook revocation).
- `role 'bot'` instead of an `is_bot` column: no change to `userCols`/Scan lists, and clients can badge on the existing `role` field.
- DM privacy: the target's `AllowPrivateMessages`/contact rules apply, except the CLI `--self` flag, which is an operator assertion that the target is the operator's own account (the CLI has no chat identity to verify). The check becomes real if webhook management is ever exposed over REST.
- Webhook text never resolves `@mentions` (`MessageInput.SkipMentions`, `json:"-"`), so inbound mail cannot ping users; normal push is unchanged.

Reason: a leaked secret can then only post into one room as an identifiable bot.

## ADR-021: Webhook secret in the URL path, hashed, with log redaction

`POST /hooks/{id}/{secret}`; secret from `id.NewToken()`, stored as `id.HashToken`, constant-time compare, optional CIDR allowlist, rotation via CLI with no overlap window. Postmark inbound has no signature and cannot set headers; the only options are credentials in the URL or an IP allowlist, and the path form serves both Postmark and the Worker with one scheme.

- Accepted weakness: the secret appears in the request line, so app logging (`withLogging`, `withRecover`) must redact it and the reverse proxy/CDN must mask `/hooks/` paths. A header secret is the upgrade for sources that can set one.
- All auth failures return the same 403 (it also stops Postmark retries).

## ADR-022: Webhook attachments are ingested server-side as bot-owned attachments

Email attachments arrive as base64 inside the JSON body and are stored through the existing attachment pipeline: `CreateUpload` (sanitizing, mime and size checks, server-generated key) then `Storage.PutObject` then `Complete`, all as the bot principal. `Storage` gains `PutObject` (already implemented on `S3`).

- A bad or oversize attachment becomes a line in the message body, never a failed request, so mail is not lost.
- Limits: 10 attachments per message (existing), `MAX_UPLOAD_BYTES` per file, `WEBHOOK_MAX_BODY_BYTES` per request, 2 concurrent ingests. Base64 JSON is held in memory about three times during decode; streaming decode is the upgrade path.
- Rejected alternatives: dropping attachments (user chose full fidelity) and a second bypass storage path (duplicates the validation).
- Message text is rendered inside a code fence with inline-code headers because `markdown.Validate` rejects raw HTML-like text (`<a@b.c>`) that every email contains.
