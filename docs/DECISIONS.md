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

> Superseded in part by ADR-026: admins may now invite by email.

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

## ADR-023: Rocket.Chat live bridge — per-user PAT delegation, mirror rooms, polling

`internal/rcbridge` continuously mirrors a running Rocket.Chat workspace into local rooms (docs/ROCKETCHAT-INTEGRATION.md was the requirements pass; research + open questions there). Choices:

- **Delegation, not bots**: each user pastes their own RC Personal Access Token (`users.generatePersonalAccessToken`, permission `create-personal-access-tokens`) + RC userId via `POST /api/v1/integrations/rocket-chat/link`. The PAT authenticates both REST and any future realtime socket (`login {resume: <PAT>}`); no RC-side admin, bot account, or federation. Federated routes (native Matrix federation = v7.11 Premium) were rejected as server-to-server scope beyond the goal.
- **Model A mirror** over fetch-through proxying: bridged rooms are real local rooms (`rooms.source = 'rocket_chat'`, `source_rid`, `source_synced_at` per-room watermark in 005). WS delivery, push, receipts, search, pagination need no special-casing; only posting routes remotely (`MessageService.Create` delegates to `App.RC.Send`).
- **Shared id space**: room/message/author uuidv5 ids come from the same namespaces as ADR-019 (`internal/rcids`), so linking a previously `rcmigrate`-imported workspace reopens identical rooms and `ON CONFLICT (id)` dedupes — no cutover migration.
- **RC identities**: remote authors become non-login stub users (`password_hash = '!'`, like webhook bots) unless the same `nsUser` id exists as an imported real user. The link owner's own RC account maps to their real local user. RC usernames colliding with local accounts get `-rc` suffixes.
- **Transport**: REST deltas only in v1 — `subscriptions.get?updatedSince` per link (watermark = max `_updatedAt` seen), `dm.history`/`groups.history` per dirty room, 100-message backfill on first sight, one 30 s poll goroutine for all links (sequential; shared rate-limit budget makes fan-out counterproductive). Endpoint-name renames (`im.*`→`dm.*`) handled by try-and-remember fallbacks. RC websocket streams are the known upgrade when push latency matters.
- **Send-through**: `chat.postMessage` with the user's PAT; local row is the RC echo's deterministic id, so the sync race with the echo dedupes by construction. Thread replies (`tmid`), edits/deletes back, reactions, attachments, `t=c` channels: out of scope v1, fail closed on the local side.
- **Unlink** destroys the stored PAT (AES-256-GCM, key from `RC_PAT_KEY`, required whenever `RC_BASE_URL` is set); mirrored rooms remain as frozen local history.
- **Read state stays one-way**: reading on RC does not propagate; receipts are local-only. Limits mirror webhook conventions (never log the PAT, never echo it).

## ADR-024: Rocket.Chat bridge is per-user — own workspace URL, owner-keyed rows

Amends ADR-023 after a review found server-wide state where the data belongs to one user.

- **Per-user workspace**: `rocket_chat_links.base_url` (migration 006); `POST .../link` takes `base_url` (a deployment default `RC_BASE_URL` is optional). `RC_PAT_KEY` is now the feature switch. Every outbound call passes `push.CheckHost` (private-address rejection, `RC_ALLOW_HOSTS` allow-list, no redirects) because the URL is user input; endpoint-name caches are keyed per workspace.
- **Owner-keyed rows**: room/message/shadow-user ids = uuidv5(owner, workspace, remote id) (`rcbridge.rcID`). No row is shared between users — fixes the second user never joining a shared room, per-room watermarks shared across users, and one user's remote contacts leaking to others. Shadow accounts (`password_hash '!'`) are excluded from user search. Mirrored rooms refuse member adds/invites (they post through the owner's token; `Send` also requires `created_by = caller`).
- **Unlink hides**: `DeleteRCLink` destroys the PAT and archives the owner's mirrored rooms in one transaction; history is kept and a re-link restores the same rooms (deterministic ids).
- **Import continuity dropped**: live ids no longer share the `rcmigrate` namespaces, so an imported workspace is not adopted by the bridge (it would also pin the id to a single owner). Revisit with an explicit adopt step if needed.
- **Bugs fixed**: delta pulls re-fetched the full window (`source_synced_at` was never read back); DM history called `groups.history` (type compared as `"d"` against `"direct"`); `svc.RC` was a typed-nil interface when disabled.
- **Not done (ponytail)**: delete-my-mirror, per-user sync interval / type filters, per-workspace workers, HKDF per-user token keys.


## ADR-025: Passkeys (WebAuthn) as a passwordless login; Android via browser handoff

Design in `docs/PASSKEYS.md`.

- **Passwordless alternative, full MFA**: a discoverable passkey (resident key, user verification required) signs in alone and skips the TOTP challenge. Password (+TOTP) login is unchanged. Library: `go-webauthn/webauthn`; tests use `descope/virtualwebauthn`.
- **RP from `ORIGIN`**: RP ID = origin host, one allowed origin. `ORIGIN` unset → feature off (`404 passkeys_disabled`). Changing the host invalidates every registered passkey.
- **Storage**: `user_passkeys.credential` is the library's Credential as JSONB (sign count, flags, key stay library-owned); `credential_id` is lifted out for lookup. Ceremony state lives in `webauthn_ceremonies` (Postgres, single-use, 5 min), not process memory. User handle = the user's UUID bytes (no new `users` column).
- **Registration needs re-auth**: password, plus TOTP/backup code when 2FA is on, passed inline (same shape as `DisableTOTP`) — no "recently verified" server state. Sessions only; shadow accounts refused; max 10 passkeys. A clone warning (sign-count regression) fails the login and is audited.
- **Last-passkey guard**: deleting a user's last passkey is refused (409) when the account has no usable password. Dead code until passwordless accounts exist; kept on purpose.
- **Android = browser handoff, not Credential Manager**: Credential Manager needs `/.well-known/assetlinks.json` per server domain and per signing key (F-Droid/Play/debug differ). Instead the app opens a Custom Tab on the web login; after any login the page mints a 60 s one-time code bound to the app's PKCE challenge (`/auth/mobile/code`) and deep-links it; the app redeems it (`/auth/mobile/token`) for a normal API token. An intercepted deep link yields a code that is useless without the verifier.
- **Rate limits**: passkey `finish` and the code exchange share a dedicated per-IP bucket (`passkeyRate`, same ceiling as password login); `begin` only the global bucket.
- **Not done (ponytail)**: attestation / FIDO MDS (attestation `none`), related-origins, passkey-only enforcement, step-up on delete/rename.

## ADR-026: Email signup invites (supersedes ADR-014's "no email, CLI-only accounts")

Design in `docs/SIGNUP_INVITES_DESIGN.md`; requirements in `docs/SIGNUP_INVITES_REQUIREMENTS.md`.

- **Admin-initiated only**: an admin (REST, CLI `signup-invite`, webui Admin page) emails a one-time link; there is still no open registration, no password reset and no email verification beyond "you hold the link". CLI `user create` remains.
- **Token**: 256-bit, stored hashed (`signup_invites.token_hash`), single-use, 7-day expiry, revocable. The link carries it in the URL fragment (`ORIGIN/signup#token=…`) so it never reaches server logs or Referer; the API takes it in a POST body. Unknown/expired/used/revoked are indistinguishable (404).
- **Consume + create in one transaction**: a row-locking `UPDATE … WHERE unused` makes concurrent submits yield one account; validation failures roll back and leave the token live. Role is always `user`; the email comes from the invite row.
- **Resend = re-key**: one open invite per email (partial unique index); inviting an open email or `resend` rotates the token and resets expiry. `CreateUser` revokes an open invite for its email in the same transaction.
- **Email transport**: `service.Mailer` (real boundary, faked in tests); stdlib `net/smtp` implementation in `internal/mail`. **SMTP settings are admin-managed in the UI**, not env: host, port, user, password, TLS mode and From live in `app_config` key `mail.smtp` (`GET/PUT /admin/mail-settings`, `POST …/test`), applied without a restart. The password is AES-GCM sealed with `SECRETS_KEY` (env; unset = settings cannot be saved, feature off); it is never returned by the API. The host passes `push.CheckHost` on every send (`MAIL_ALLOW_HOSTS` exempts self-hosted relays). Saved host + key = signup routes live; clearing the host switches them off (404). SMTP auth is refused over `tls=none`. `ORIGIN` must be set to build the link. A Postmark HTTP sender is a later drop-in.
- **Abuse guard**: public routes share the per-IP login bucket; Turnstile (when configured) on `POST /signup`.
- **Password rule (signup only)**: 12–128 characters, no composition rules.
- **`users.username` uniqueness**: migration 008 adds the missing `UNIQUE` index (001 only had a CHECK, so the CLI's conflict path never fired). It fails if duplicates exist — resolve by hand.
- **Not done (ponytail)**: async mail queue (send is synchronous, 15 s bound), HTML mail, per-invite role/rooms, Postmark sender.
