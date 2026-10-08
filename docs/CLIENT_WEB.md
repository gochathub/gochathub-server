# Web Client Agent Brief

Client repository for AI agents building/maintaining the web UI. The client is
a separate repository; this document plus the server contract are the
authoritative inputs. Client agents should read this top to bottom before
writing code.

## The one rule about truth

`api/openapi.yaml` (this repo) is the normative request/response contract.
`docs/DECISIONS.md` ADRs and `docs/WEBSOCKETS.md` own behavior.
`scripts/contract-check.py` enforces contract↔server sync. When any of these
conflict with this document, the contract/ADRs win.

## Baseline

[Avian-Template](https://github.com/daemon-bixia/Avian-Template) (Vue 3 + Pinia
+ vue-router + Tailwind 4, Vite). It is a UI template: every value is mocked
from `src/store/defaults.ts`, there is no HTTP client, no WebSocket, no auth
logic. All integration is ours to build in the fork.

Fork decisions already made (do not re-litigate; see `docs/WEB_CLIENT.md`):

| Feature | Action in the fork |
| --- | --- |
| Voice calls (dialer, call list, modals) | Strip entirely (`Call*.vue`, `DialModal`, `VoiceCallModal`, `CallInfo*`, wavesurfer calls UI) |
| Signup / password reset pages | Strip (`RegisterForm`, `PasswordResetView`) — accounts are CLI-managed server-side (ADR-014) |
| Link previews (`previewData`) | Strip rendering; keep client-side linkifying (linkify-string is already a dep) |
| Broadcast conversations | No special type: render as group rooms |
| Numeric IDs (`id: number`) | Change to string everywhere; IDs are opaque UUIDs |
| Email login field | Login is by username |
| `localStorage.token` | Deleted — the cookie replaces it (ADR-015); strip the token handling in `AccessView` flows |

## Authentication

- Login: `POST /api/v1/auth/login {username, password}` → 200 with the user
  JSON plus a `Set-Cookie: chat_session` (httpOnly, ADR-015). The browser
  client never sees or stores a token; never read the body for credentials.
- Logout: `POST /api/v1/auth/logout`; the server also clears the cookie.
- CSRF posture: the server enforces `SameSite=Lax` + origin checks; the client
  must not add cross-origin form posts. Single origin through the reverse
  proxy — there is no CORS support (ADR-016), the dev server (Vite) must
  proxy `/api` to the backend rather than talking cross-origin.
- Every request relies on the cookie; no bearer header in the web app.

## Where the integration code goes

New fork code lives in clean, small modules — not inside UI components:

```
src/api/          fetch wrapper: base path /api/v1, JSON, error envelope unwrap
src/api/schemas   TS types generated from openapi.yaml (openapi-typescript)
src/ws/           WebSocket client (below)
src/store/        Pinia stores backed by the API (replace defaults.ts mocks)
```

Generate TS types from the contract instead of hand-writing interfaces:
`openapi-typescript` (schema-types only). Keep `src/@custom_types` additions to
libraries the template missed.

## Error envelope

All non-2xx responses are:

```json
{ "error": { "code": "not_found", "message": "..." } }
```

Handlers should switch on `code` (stable strings: `unauthorized`, `forbidden`,
`not_found`, `conflict`, `validation`, `rate_limited`, ...) for UX decisions;
`message` is human-readable. Request correlation: echo `X-Request-ID`.

## Pagination

Message listing is cursor-based: `GET /rooms/{id}/messages?limit=50&before=<cursor>`;

```json
{ "items": [...], "next_cursor": "..." }
```

Load older history by following `next_cursor`; newest page first (no cursor).
Never offset-paginate.

## Identifiers, timestamps, locale

- IDs: opaque strings.
- Timestamps: RFC 3339 UTC on the wire. The server sends `timezone` (IANA
  name, e.g. `Europe/Berlin`) in user payloads; render date/time in the
  viewer's local timezone, use the peer's `timezone` hint only for
  locale-aware grouping where the UI calls for it.

## WebSocket client

Endpoint: `GET /api/v1/ws` (same origin; the session cookie authorizes the
upgrade automatically). Frame and event shapes are in `docs/WEBSOCKETS.md`;
summary for the client work:

- On connect the server sends `{"type":"connected", ...}`.
- Client sends `{"type":"subscribe","room_id":...}` (membership enforced).
- Incoming events: `message.created/updated/deleted`, `message.receipts_changed`,
  `message.reaction_added/removed`, `room.pinned_changed`,
  `room.read_state_changed`, `room.member_added/removed`, `room.*`,
  `contact.added/removed/updated`, `invite.*`, `presence.changed`,
  `typing.started/stopped`.
- Client emits `{"type":"ack","message_ids":[...]}` for delivered receipts,
  `{"type":"read","room_id":...,"message_id":...}` for read cursor, and
  typing frames; heartbeats are transparent.
- Reconnect: on drop, refetch authoritative state over REST (rooms list,
  message pages, read state), then re-subscribe. Do not build an event
  replay log.

## Receipts rendering (ADR-009)

The web client renders per-message state:

- The recipient's OWN receipt is in `message.receipts` (`delivered_at`,
  `read_at`, nullable timestamps).
- For messages you SENT (author = you), `receipts` carries the aggregate for
  all receipt-participating members: `read_at` non-null = everyone read
  (double check), `delivered_at` non-null = everyone delivered (double
  ticks), nulls = pending.
- The `readReceipts` preference gates aggregation server-side; when a reader
  opts out, your sent messages may stay "delivered" — that is intended.

## Data mapping (Avian model → server)

| Avian | Server |
| --- | --- |
| `IConversation` | `Room` (`type`: public/private/direct/group_direct; `name` for groups/directs; direct rooms have null name) |
| `contacts` list | `GET /contacts` (ADR-010) |
| `messages` | `GET /rooms/{id}/messages` (cursor) |
| `draftMessage` | client-local state (never sent) |
| `unread` | `Room.unread_count` on room list |
| `pinnedMessage` | `Room.pinned_message_id` + message fetch; "hide pin" is client-local |
| `archivedConversations` | per-member archive flag server-side (member-level archive endpoint) + `Room.archived_at` for room archive; keep both views |
| `INotification` sidebar | derive from WS events (`invite.created`, `room.member_added`, security events are not pushed to web; do not invent a notifications REST endpoint) |
| avatars | `avatar_url` (presigned URL, may be a Gravatar link, or empty → monogram with initials) |

## Message content rules

- Bodies are markdown from the server-validated subset (see
  `internal/markdown` + `docs/REQUIREMENTS.md`): bold/italic/strike, inline
  and fenced code, links, quotes, lists, headings, `@mentions`.
- Raw HTML never appears in bodies. Render markdown to safe DOM on the client
  (the server rejects unescaped tags outside code spans).
- Mentions: `@username` tokens; server parses and stores them; client renders
  highlight by `@username` token.

## Attachment flow

1. `POST /attachments {filename, mime_type, size_bytes, sha256?}` → returns
   `{attachment, upload_url}` (presigned PUT URL).
2. `PUT upload_url` with the raw file body (S3 does not talk to the API).
3. `POST /attachments/{id}/complete` → 200 when verified.
4. Attachment payloads in messages include `url` (short-lived presigned GET) —
   refetch message data on 403/expiry; the URL is not a stable link.

Direct-to-storage means the server never streams file bytes; honor
`MAX_UPLOAD_BYTES` before uploading.

## What the client agent must NOT build

- No CORS handling (single origin, ADR-016).
- No signup, password reset, email verification UI (ADR-014).
- No voice-call UI (ADR-012).
- No push beyond the server's own Web Push (`platform: web` device via
  `/push/vapid` + `/devices`, shown by `src/sw.ts`) and the WS + browser
  Notification API fallback. No FCM/APNs integration in the client.
- No client-side business rules that the server already enforces
  (authorization, receipt aggregation, moderation).

## Testing expectations for the client repo

- A smoke test that logs in with a seeded account, lists rooms, posts a
  message, receives the WS event, marks read — against a running server
  (env-configured base URL, skipped when unset).
- Type generation checked into the repo; CI fails on contract drift
  (`python3 ../gochatserver/scripts/contract-check.py` or pin the openapi
  snapshot).
- Deterministic tests otherwise (no wall-clock assertions beyond RFC 3339
  parsing).