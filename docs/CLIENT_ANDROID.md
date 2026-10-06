# Android Client Agent Brief

Client repository for AI agents building/maintaining the Android app. Client
is a separate repository; this document + the server contract are the
authoritative inputs.

## The one rule about truth

`api/openapi.yaml` (server repo) is the normative contract; `docs/WEBSOCKETS.md`
owns the realtime protocol; `docs/DECISIONS.md` ADRs own behavior decisions;
`docs/UNIFIEDPUSH.md` owns the push contract (verified live 2026-10-06).
Where this file conflicts with those, those win.

## Baseline

[CometChat Android UI Kit](https://github.com/cometchat/cometchat-uikit-android)
(`chatuikit-compose` for Jetpack Compose UI over `chatuikit-core`).

Reality check from the repo inspection — the Kit is **UI + ViewModels +
per-screen data sources** on top of the closed CometChat cloud SDK
(`com.cometchat.chat.*`, `api(libs.chat.sdk.android)`). The SDK's networking
talks to CometChat's cloud and **cannot be pointed at our server**. The
`calls-sdk-android` artifact is `compileOnly` (absent from POMs) and we do not
use calls (ADR-012).

### Fork strategy (decided)

Fork the Kit and replace the SDK networking layer, keeping the UI:

1. Keep the Compose components, ViewModels, and theme.
2. The Kit's seam is its datasource layer:
   `chatuikit-core/.../core/data/datasource/*` (e.g.
   `ConversationListDataSource`, message/group/user data sources) defines the
   contracts ViewModels consume. Implement them against our REST/WS client.
3. Audit every `com.cometchat.chat` import in the fork
   (`grep -rn "com.cometchat.chat"` is the first commit in the new repo):
   model classes stay (data holders), networking/statics get replaced. Where
   a ViewModel calls SDK statics directly, route it through the datasource
   seam instead.
4. Delete the calls modules and any `calls-sdk` references (ADR-012).
5. If the SDK artifact turns out to be non-trivial to keep (its models drag
   in cloud protocol), mirror the needed model classes in
   `data/model/` and map at the datasource boundary — prefer this over
   shipping a cloud SDK that never connects.

Login/account: no signup or password reset screens — accounts are
CLI-administered server-side (ADR-014); the app assumes the user has
credentials handed over out of band.

## Authentication

- Login: `POST /api/v1/auth/login {username, password, token_request: true}` →
  200 `{token, user}`; store the token in
  `EncryptedSharedPreferences`/Keystore. Never put it in query strings.
- Use `Authorization: Bearer <token>` on every API call and WebSocket
  upgrade.
- Logout: `POST /api/v1/auth/logout`; then wipe local stores.
- Sessions are revocable server-side (password change revokes them; the app
  must react to 401 by dropping to the login screen).

## Contract summary

- Errors: `{ "error": { "code": "...", "message": "..." } }` — switch on
  stable `code` strings, never message text.
- IDs: opaque strings (UUIDv7 — sortable).
- Timestamps: RFC 3339 UTC; the server provides the user's `timezone`
  (IANA) — render local, use peer timezone hint for display grouping.
- Message history: cursor pagination
  `GET /rooms/{roomId}/messages?limit=50&before=<cursor>` →
  `{items, next_cursor}`; newest first. `before=""` → newest page.
- Message bodies are markdown from the documented subset — render with
  markdown-to-Compose (no raw HTML ever crosses the wire outside code spans).
- Attachments: upload-session flow (`POST /attachments` → presigned PUT →
  `POST /attachments/{id}/complete`); download URLs are short-lived
  presigned GETs — on 403, refetch the message.

## Realtime (WebSocket)

`GET /api/v1/ws` with bearer auth header on the upgrade. Frames + event
envelope per `docs/WEBSOCKETS.md`. Client duties:

- Subscribe to visible rooms; resync missed state over REST after drops.
- `ack` frames (`message_ids`) mark delivered receipts server-side.
- `read` frames advance the read cursor (equivalently
  `POST /rooms/{id}/read`); read receipts are per ADR-009 (sender-visible
  aggregate vs own receipt — mirror the web client's rendering rules).
- Typing relay through `typing.started`/`typing.stopped`; presence is
  delivered by `presence.changed` events; there is no separate presence
  API call.

## Push (UnifiedPush) — the verified contract

ntfy is the push provider; the user installs the ntfy Android app as the
UnifiedPush distributor (or any UP distributor). Server-side flow verified
end-to-end against the self-hosted ntfy (`docs/UNIFIEDPUSH.md`):

1. Fetch server VAPID public key: `GET /api/v1/push/vapid` (base64url, 87
   bytes P-256 SEC1 uncompressed) — pass to the connector at registration.
2. Register via the official UnifiedPush connector library
   (`android-connector`): the connector talks to the distributor, returns
   `endpoint` + generates `p256dh` key + `auth` secret.
3. `POST /api/v1/devices {platform:"android", client_name, client_version,
   push_registration:{endpoint, public_key, auth_secret}}` →
   201 `{device_id, validation_required: true}`. Registration is only
   provisionally active until validated.
4. The connector delivers the **validation ping**: the server sent a
   one-time token encrypted (RFC 8291) as a push. Decrypt (connector
   handles it) and post back:
   `POST /api/v1/devices/{device_id}/validate {token}` → 204. Now pushes
   flow.
5. Endpoint renewals: `PATCH /api/v1/devices/{device_id}` with the new
   registration; stale registrations get replaced server-side.
6. On push (`chat.message`): payload is identifiers only
   `{type, room_id, message_id}` — fetch the authoritative message over
   REST before rendering; never assume payload contains content.
7. Lifecycle: re-register on app start; a distributor may unregister after
   ~30 days without a renewal ack (~AND_3.1.0). Battery: request the usual
   exemptions for the distributor app; do not implement a distributor
   yourself.
8. No FCM inside the backend (ADR: docs/DECISIONS.md); the
   `embedded_fcm_distributor` connector add-on is out of scope.

Notification modes: `POST /api/v1/users/me/notifications {mode:
all|mentions|directs|never}` with optional `room_id` for per-room overrides —
`GET` lists current values. The server applies these BEFORE sending pushes;
the client only handles what arrives.

## Data mapping (Kit model → server)

| Kit/SDK concept | Server |
| --- | --- |
| `User` | `/users/me` (self), `/users/{id}` (visible users), contacts |
| contacts / user list | `GET /contacts`, add/remove; `GET /users/search` for lookup |
| `Conversation` (user type) | `direct` room (created via room create with `members:[userId]`, requires the peer's allow_private_messages pref or contact relation) |
| `Conversation` (group) | `group_direct`/`private`/`public` rooms + membership |
| `Group` members/admins | room members + `my_role` (member/admin) |
| invites/comet group join requests | `room_invites`: `POST /invites` (admin), accept/decline endpoints, `GET /invites` |
| `TextMessage` | message `body` (markdown subset) |
| `MediaMessage` | message `attachments[]` (metadata + presigned URLs) |
| receipts (`deliveredAt/readAt`) | `message.receipts` (ADR-009 rendering rules in CLIENT_WEB.md — same rules) |
| reactions | per-message counts + own reaction endpoints |
| `parentMessageId` threads | server models single-level replies (`reply_to_message_id`); render Kit thread UI against a 1-level thread, or flatten — do NOT build client-side thread state |
| pinned messages (Kit has pin viewmodels) | `PUT/DELETE /rooms/{id}/pin` (admin) |
| presence/status | WS `presence.changed` + `last_seen_at` (gated by peer prefs) |
| calls / call logs | DELETED (ADR-012) |

## What the client agent must NOT build

- No self-registration/account UI (ADR-014).
- No voice/video calls or WebRTC surface (ADR-012).
- No FCM/APNs direct integration (backend + deployment constraint).
- No secondary database as source of truth for chat state — a local cache for
  offline UX is fine, restore/resync always reconciles against REST/Postgres.
- No custom push distributor; use the installed one via the connector.
- No bypassing the contract: generated client types from `openapi.yaml`
  (e.g. `openapi-generator` Kotlin client or hand-mapped typed DTOs from the
  generated TS schema — pick one; do not fork JSON handling per-feature).

## Server-facing behavior the agent should assume

- Server is authoritative: after any WS drop, REST resync wins.
- Read-state and receipts are eventual: aggregate receipts may arrive late as
  `message.receipts_changed` events — render optimistically, reconcile on
  events.
- Rate limits exist (per-IP token bucket); handle `rate_limited` +
  `Retry-After` with backoff rather than retry storms.

## Testing expectations for the client repo

- Unit tests for mapping/receipt rendering/decrypt-claim handling.
- Instrumented tests against a running server (env-configured base URL):
  login(token_request) → rooms → message post → WS event → push validation
  round trip (when a distributor is available in the emulator).
- Contract drift guard: the openapi snapshot/version pinned in CI; fail the
  build on mismatch.
- Emulator targets API 28+ (Kit prerequisite).