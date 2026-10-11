# Rocket.Chat live integration — requirements discovery

Status: **v1 implemented** (2026-10-11, ADR-023, `internal/rcbridge`) — Model A (mirror), PAT delegation, REST polling, DMs + private groups, send-through. The research below stays accurate; §7's open questions 1–4 are resolved by the defaults recorded in ADR-023, and 5–7 remain open. Deferred scope is in §6. This is NOT the mongodump import (`rcmigrate`, ADR-019): that one-shot import is shipped; this document covers the live, ongoing case — rooms from a running Rocket.Chat server appearing in a GoChatHub user's room list.

## 1. The question

A user has an account on a running Rocket.Chat server and an account here.
Can their Rocket.Chat **direct messages** and **groups** appear in their local
GoChatHub room list, so the two message servers look like one inbox?
If yes, what does the Rocket.Chat API actually support for that?

**Short answer: yes — fully supported by the public API, delegately and
without admin help.** Rocket.Chat exposes everything needed: the user's own DM
and group subscriptions (with incremental sync), message history with time
range queries, message sending, and a realtime socket stream using a
long-lived Personal Access Token. No Rocket.Chat-side plugin, bot, or
server-to-server federation is required. The two viable caveats are (1) REST
rate limits constrain how aggressively the server may poll, and (2) RC's newer
endpoint renames are version-dependent.

## 2. Verified API facts

All facts below were checked against the official API reference
(developer.rocket.chat, OpenAPI-backed) and the Rocket.Chat OpenAPI spec
(GitHub `RocketChat/Rocket.Chat-Open-API`). Confidence: high unless marked.

### 2.1 Delegated authentication — Personal Access Tokens

| Fact | Detail |
|---|---|
| Token acquisition | `POST /api/v1/users.generatePersonalAccessToken` with `{tokenName}`; requires the `create-personal-access-tokens` permission and a 2FA step (`x-2fa-code` / `x-2fa-method`), unless the user creates the token in the UI or carries `bypassTwoFactor`. |
| Token lifetime | Permanent until regenerated or deleted. Survives logouts — unlike session `authToken`s, which expire per workspace policy. Irrecoverable once lost. |
| Usage (REST) | Send the PAT directly as `X-Auth-Token` with `X-User-Id` (the RC user's id, returned to the user in the UI when creating the token). No login round-trip needed. Community-verified equivalence with session tokens; official docs document PAT as "permanent authentication token … to access the API". |
| Usage (Realtime) | WebSocket `login` method accepts `{ "resume": "<token>" }` where the token may be "a personal access token or previous authToken" — so the PAT alone authenticates a live socket session. |
| Login endpoint | `POST /api/v1/login` takes `{user, password}` or `{resume}` or `{user, password, code}` (2FA); returns `{data: {authToken, userId, me}}`. Password login rotates the prior session token — one more reason to prefer PATs over stored passwords. |
| Revocation | `POST /api/v1/logout` invalidates one session token; PATs are revoked only via regenerate/delete (`users.regeneratePersonalAccessToken`, `users.getPersonalAccessTokens`). |

Consequence for the bridge design: **the user only needs to paste two strings
into GoChatHub — their PAT and their RC userId** (or username). The PAT works
for both REST and realtime. Storing their password is never required.

### 2.2 Listing the user's rooms (DMs and groups)

The single most useful endpoint is the user's own subscription list — it covers
all room types at once and supports incremental sync:

| Endpoint | Purpose | Notes |
|---|---|---|
| `GET /api/v1/subscriptions.get[.list]` | Every room the user is a member of (channels `c`, private groups `p`, DMs `d`; `l` = livechat, ignore) | `updatedSince` (ISO 8601) turns it into a **delta**: response becomes `update` + `remove` arrays (`remove` items carry `_id`, `_deletedAt`). Fields include `rid`, `name`, `t`, `open`, `alert`, `unread`, `userMentions`, `groupMentions`, `ls`/`lm`, `_updatedAt`. This is the intended sync primitive. |
| `GET /api/v1/dm.list` (older workspaces: `im.list`) | Own DMs | `offset`/`count`/`sort` pagination; no `updatedSince`. |
| `GET /api/v1/groups.list` | Private groups the user belongs to | Pagination only; no `updatedSince`. |
| `GET /api/v1/channels.list` | Public channels the user has joined | Pagination only. |
| `(admin) GET /api/v1/im.list.everyone` | All DMs workspace-wide | Requires `view-room-administration` — **not** used by per-user delegation. |

Version note: RC renamed the `im.*` family to `dm.*` on newer majors (the docs
show both, OpenAPI `operationId`s keep `im.*`). The bridge must try the new
name and fall back to `im.*`, or discover the version once at link time.
Confidence: high on the rename existing; moderate on the exact version where
each landed.

Unread state per room comes free from the subscription (`unread`, `alert`,
`userMentions`, `groupMentions`) — enough to render badges without touching RC
receipts.

### 2.3 Pulling messages

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/dm.history` (`im.history`) | `roomId` required; `latest`, `oldest` (ISO 8601), `inclusive`, `count`, `sort`, `unreads`, `showThreadMessages`. Returns `messages[]` (`_id`, `rid`, `msg`, `ts`, `u{_id,username}`, `t` system-type, `tmid` thread parent, attachments, `editedAt`). Same time-range contract exists as `groups.history`. |
| `GET /api/v1/dm.messages` / `groups.messages` | Message listing variants (paginated newest-first). |
| `POST /api/v1/chat.postMessage` | `roomId` (or `channel`) + `text` (+ `attachments`, `tmid`, optional `alias`/`avatar` behind `message-impersonate`). Returns the created `message` object (`_id`, `ts`, `rid`, `u`). |

Message history endpoints are the backfill mechanism. `oldest`/`latest` map
cleanly onto a per-room watermark cursor.

### 2.4 Realtime (optional, for live delivery)

WebSocket at `wss://<workspace>/websocket`:

1. `{"msg":"connect","version":"1","support":["1"]}`; answer server `ping` with `{"msg":"pong"}`.
2. `login` with `{ "resume": "<PAT>" }` → returns user id, token, expiry.
3. `sub` to `stream-notify-user`, first param `"\<userId>/subscriptions-changed"` (room list changes), `"\<userId>/rooms-changed"`, `"\<userId>/notification"` (notification payloads), `message` (incoming DM ping). Payloads arrive as Meteor-style arrays (`type`, object).

The "Method Calls" layer itself is documented as deprecated in favor of REST,
but **stream subscriptions are not deprecated** — that is how RC's own clients
learn about new messages. A bridge can run REST-only (polling) or REST +
`stream-room-messages` for instant fan-out. Confidence: high on the protocol
shape, moderate on long-term support given the deprecation banner on method
calls.

### 2.5 Rate limits

REST rate limiter is enabled by default; the shipped default is **90 calls per
60 s per IP+route** (workspace-configurable, example configs use 10). 429
responses carry `X-RateLimit-Limit` / `-Remaining` / `-Reset` headers.
A bridge polling one user per, say, 30–60 s (2 calls: subscriptions delta +
histories for changed rooms) sits orders of magnitude below the default.
Polling all users from one server IP concentrates requests there — the
`X-RateLimit-Reset` header must drive backoff.

### 2.6 What is deliberately NOT viable

| Approach | Why not |
|---|---|
| RC native federation (Matrix protocol) | Introduced v7.11, **Premium license add-on**; requires GoChatHub to implement a Matrix server-server homeserver (Ed25519 signing, `/_matrix/federation/*`). Massive scope, wrong direction for "my account links to my RC". |
| Legacy RC federation protocol v1 | Also server-to-server, DNS-based, requires the RC workspace to enable it; same scope problem. |
| Single bot/service account invited to rooms | Only sees rooms the bot was added to; **cannot see other users' DMs** (RC access control blocks it); requires manual invitation per room. Per-user PAT delegation has none of these problems. |
| Browser→RC direct (client does the bridging) | Cross-origin calls against RC, PAT managed in a third client in browser storage, and the Android client (UnifiedPush) cannot talk to a second origin cleanly. Violates the single-vendor-server architecture in docs/ARCHITECTURE.md. |
| Webhooks | One-way, push-only, misses room lists and history. |

## 3. Candidate integration models

All three are *server-side* GoChatHub features. The open architecture decision
(hence `/sc:design`) is mostly A vs B. The API supports all of them.

### Model A — mirror into local tables (persisted bridge)

Each RC room becomes a real local room; each RC message becomes a real local
message row with a deterministic id; the bridge continuously pulls deltas.

- Room ids derive with `uuid.NewSHA1(nsRoom, rid)` — the **same namespaces the
  shipped import already uses** (`internal/migrate/rocketchat.go:31-34`).
  If `rcmigrate` already imported that RC server, the live bridge opens the
  identical rooms and `ON CONFLICT (id)` dedupes — the cutover becomes free.
- Message ids derive the same way (`nsMsg(rcid+msgId)`), so backfill and
  migration cannot double-insert.
- RC authors render as local stub users (`nsUser(rcUserId)`), matching how the
  import already created users.
- Local writes fan straight into the existing pipeline: WebSocket envelopes,
  `Sender.NotifyMessage` push (UnifiedPush), receipts, search, pagination all
  Just Work — no client change beyond a small marker on the room.
- Costs: duplicate storage of RC content; deleted/edited RC messages need
  tombstone sync (`subscription remove` + `editedAt`); read state stays
  separate (read only what RC reports, don't push receipts back).

### Model B — proxy rooms (fetch-through, no local persistence)

Room list is served locally, but message bodies are fetched from RC on read
and translated on the fly.

- Pros: no duplicate storage, always live truth, no backfill.
- Cons: every client flow (list page, WS delivery, push, search, unread
  counters, sync-after-reconnect) needs a bridged-room special case with an
  RC round-trip; push payloads must still be pre-rendered; "PostgreSQL is
  authoritative" becomes false for a chunk of the room list. Higher total
  complexity across more surfaces — the opposite of minimal.

### Model C — one-way push-only bridge

Ingest only notification events via RC into local unread badges; message read
requires hopping to the RC client.

- Cheapest, but does not actually unify the inboxes; only unify the *notify*.
  Mentioned for completeness; fails the stated goal.

**Recommendation (not yet decided): Model A**, for id-space continuity with
the shipped import and because the whole downstream (WS, push, receipts,
OpenAPI) needs zero special-casing per surface. Poll REST deltas as the v1
transport; add the RC websocket stream later if delivery latency matters.
Open question §7 decides this.

## 4. Functional requirements

User stories first, then the derived requirements.

**US-1** — As a GoChatHub user with a Rocket.Chat account on `rc.example.com`,
I link the account once and see my RC DMs and private groups appear in my room
list, interleaved with native rooms.

**US-2** — Messages that arrive on Rocket.Chat show up in the linked room here,
with the RC author attributed, without me opening the RC client.

**US-3** — I can reply into a linked room from GoChatHub and it is posted to the
RC room as me.

**US-4** — If I unlink the account, linked rooms and their content are visibly
marked (or hidden) and the stored PAT is destroyed.

**US-5 (import continuity)** — If the same RC server was `rcmigrate`-ed earlier,
linking my live account continues the same rooms/messages rather than
duplicating them.

Derived requirements (model-independent; refine after A/B is chosen):

| # | Requirement |
|---|---|
| FR-1 | Per-user RC link: username-or-userId + PAT, entered in account settings (HTTP API + OpenAPI schema), revocable by the user and by CLI/HTTP admin. Exactly one RC server per deployment (admin-configured base URL). |
| FR-2 | PAT stored per user, never logged, never returned by any endpoint after creation. |
| FR-3 | Room list: each RC subscription (`t=d`, `t=p`) surfaces as a local room; DM rooms render with the other party's name/avatar; groups keep RC name. RC channels (`t=c`) opt-in or out of scope — open question. |
| FR-4 | Sync: incremental pull via `subscriptions.get?updatedSince` watermark; per-room message backfill + delta via history endpoints with a per-room watermark. Deleted subscription ⇒ room handled per US-4 semantics. |
| FR-5 | Sending: a message posted in a linked room forwards to RC `chat.postMessage`; the resulting RC message id maps back so the bridged echo does not re-create a local copy. |
| FR-6 | Identity: RC authors map to deterministic stub users using the existing uuidv5 namespaces; stub users are non-login, flagged, and upsert-safe against users created earlier by `rcmigrate`. |
| FR-7 | Failure handling: RC unreachable ⇒ stale-but-readable content with a degraded marker; 429 responses honor `X-RateLimit-Reset`; token revoked (401) ⇒ link marked invalid, user re-links. |
| FR-8 | Delivery into local clients goes through the native path (local WebSocket envelope + UnifiedPush notification), not RC's notification stream, so both clients stay RC-unaware except for a room marker. |
| FR-9 | Admin controls: enable/disable the integration feature flag; configure the RC base URL; see link counts; unlink any user. |
| FR-10 | Contract: every new request/response shape enters `api/openapi.yaml` — `scripts/contract-check.py` must stay green. |

## 5. Non-functional requirements

- **Security**
  - PAT is a long-lived credential for the *user's* account: treat as a
    secret — encrypt at rest with a server key, scrub from logs and error
    chains, never echo in responses (mirrors existing webhook-secret
    discipline and §8 secret handling in the project instructions).
  - The RC base URL is **admin config**, not user input — per-user URLs would
    make users point the server at arbitrary hosts (SSRF surface the project
    already guards against in push/send paths). Private-address rejection
    applies like the ntfy allowlist does (docs/UNIFIEDPUSH.md §3.4).
  - Outbound calls carry only the linked user's PAT — no cross-user access is
    possible even if RC is compromised in the other direction.
- **Rate limits** — default 90 req/60 s per endpoint from one source IP; delta
  poll at ≥30 s intervals scales to the deployment sizes RC ships by default.
  Batch histories only for rooms whose subscription actually reported a delta.
- **Performance** — one goroutine per linked user; no global polling storm.
  Backfill is lazy (first open of a room), not eager.
- **Correctness** — sync state is plain rows in PostgreSQL (watermarks, room
  mapping), reviewable SQL, recoverable after crashes by re-pulling the last
  watermark. PostgreSQL stays authoritative for the local copy.
- **Clients** — web (gochathub-webui) needs at most a small "source: rocket
  chat" marker to label rooms; Android needs nothing beyond the existing push
  pipeline.

## 6. Out of scope for a first version (defer, don't design)

- Attachments crossing the bridge in both directions (RC media upload is a
  3-legged UFS flow; RC-proxied image URLs would also hand RC the viewer's IP
  — proxying media through GoChatHub attachments is the eventual answer, and
  it is neither small nor demanded by the core question).
- Editing/deleting messages back into RC; RC reactions; RC threads beyond
  mapping `tmid` onto `reply_to_message_id`; typing indicators; presence;
  E2E/OTR; livechat rooms (`t=l`); discussions; multiple RC servers per user;
  migrating read receipts back to RC.
- Anything touching RC-side admin (roles, `users.createToken`) — delegation is
  strictly each user's own PAT.

## 7. Open questions (block design, not research)

1. **Model A vs B** (mirror vs proxy): recommendation is A (§3) — confirm.
2. **Backfill depth**: how much history is pulled when a linked room is
   first opened — last N messages, since-link-time only, or full history?
3. **Public channels** (`t=c`): include in v1 or DMs+private groups only? *(Resolved: included as owner-only private rooms, ADR-028.)*
4. **Live delivery**: REST delta polling only for v1 (recommended), or wire the
   RC websocket `stream-notify-user` in the same phase?
5. **Link entry UX**: user pastes PAT + userId (self-service, needs RC admin
   to grant `create-personal-access-tokens`), or the RC workspace admin runs a
   provisioned token flow? Affects FR-1 wording and error messaging.
6. **Unlink semantics**: hide linked rooms vs show-frozen tombstone (US-4).
7. **2FA on PAT creation**: if the RC account enforces 2FA, token creation from
   the RC UI still works for the user — confirm the target workspace allows
   PATs at all (permission gate), else the feature needs a bot/admin fallback
   that does NOT read DMs — i.e., feature fails closed.

## 8. Sources

- API reference root: https://developer.rocket.chat/apidocs (+ `llms.txt` page index; `.md` mirrors of pages used)
- Authentication: https://developer.rocket.chat/apidocs/authentication-api , `RocketChat/Rocket.Chat-Open-API` `authentication.yaml` (login `user`/`password`/`resume`/`code`, logout)
- PATs: https://developer.rocket.chat/apidocs/generate-personal-access-token (permission + 2FA gate), https://developer.rocket.chat/apidocs/get-personal-access-tokens , https://developer.rocket.chat/apidocs/regenerate-personal-access-token
- Subscription sync: https://developer.rocket.chat/apidocs/subscriptions-api , https://developer.rocket.chat/apidocs/get-all-subscriptions (`updatedSince`, `update`/`remove` arrays)
- Own DM list / history: https://developer.rocket.chat/apidocs/list-dms (`GET /api/v1/dm.list`), https://developer.rocket.chat/apidocs/dm-history (`latest`/`oldest`/`inclusive`/`unreads`)
- Groups: https://developer.rocket.chat/apidocs/get-groups (`GET /api/v1/groups.list`)
- Send: https://developer.rocket.chat/apidocs/post-message (`POST /api/v1/chat.postMessage`, `message-impersonate` note)
- Realtime: https://developer.rocket.chat/apidocs/realtimeapi (connect/ping), https://developer.rocket.chat/apidocs/login-realtime (`resume` accepts "personal access token or previous authToken"), https://developer.rocket.chat/apidocs/notify-user-realtime (`stream-notify-user` events: `message`, `notification`, `rooms-changed`, `subscriptions-changed`)
- Rate limiting: https://docs.rocket.chat/docs/rate-limiter and https://developer.rocket.chat/apidocs/rocketchat-api (429 + `X-RateLimit-*` headers)
- Pat-as-header usage equivalence: forums.rocket.chat threads corroborating `X-Auth-Token` = PAT (community-verified; confidence moderate)
- Native federation (rejected alternative): https://docs.rocket.chat/docs/federation-architecture-and-capabilities , https://github.com/RocketChat/homeserver