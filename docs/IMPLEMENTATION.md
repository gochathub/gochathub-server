# Feature Checklist

Development happens in one continuous pass — not phase-by-phase (see `CLAUDE.md`). Headings are feature groupings, not stage gates; this checklist is the completion tracker.

Checked = implemented and covered by the test suite (unit + integration + live-ntfy, 2026-10-06). Repo pushed: https://github.com/gochathub/gochathub-server (main).

Remaining open items (unchecked below): image dimension extraction,
attachment object janitor, WebSocket LISTEN/NOTIFY for multi-instance,
on-device Android distributor test, idempotency keys, rate-limit tests.

## Foundation

- [x] Initialize Go module.
- [x] Establish project layout (`cmd/chatserver`, `internal/*`, `db/migrations`).
- [x] Configuration loading (env, `internal/config`).
- [x] Structured logging (slog).
- [x] PostgreSQL connection pool (pgx/v5).
- [x] Migration framework (goose, embedded; never implicit at startup).
- [x] Health/readiness/version endpoints.
- [x] CLI (cobra): migrate/serve/version/user/room/token.
- Constraint: Docker is NOT required — do not add it unless explicitly requested (handled in out-of-scope below).

## Identity

- [x] Users.
- [x] Argon2id password hashing (hostile-hash bounds).
- [x] Login/logout (cookie flow, ADR-015).
- [x] Sessions — httpOnly cookie for browsers, same-origin checks; bearer for Android/CLI/integrations.
- [x] API tokens.
- [x] Authentication middleware.
- [x] Authorization primitives (membership/role checks, visibility rules).
- [x] CLI user management (create/list/passwd/enable/disable/delete).
- [x] Accounts are CLI-managed only — no self-registration/password reset/email (ADR-014).

## Rooms and messages

- [x] Public/private rooms; direct and group-direct rooms.
- [x] Membership, roles, invitations (respecting prefs + open-invite uniqueness).
- [x] Message persistence, Markdown/rich-text validation (documented subset, `internal/markdown`).
- [x] Cursor pagination (`(created_at, id)`, opaque base64 cursor).
- [x] Edit/delete (revision history, tombstones), reactions, mentions (@username), replies.
- [x] Message pinning (`/rooms/{id}/pin`, admin-only).
- [x] Per-message delivery/read receipts (`message_receipts`, ADR-009; sender-aggregate/own-receipt visibility).
- [x] Read state and unread counts (`last_read_message_id` cursor).

## Contacts

- [x] `contacts` table (ADR-010): add/remove/list, one-way relationships.
- [x] User search endpoint (`GET /users/search`).
- [x] WebSocket contact sync events.

## Preferences

- [x] `users.preferences` JSONB (ADR-013): defaults functionality-on/privacy-off.
- [x] `GET/PATCH /users/me/preferences`.
- [x] Last-seen and read-receipt visibility enforced server-side (payloads + receipt aggregation).

## Avatars

- [x] `avatar_attachment_id` on users (and rooms) referencing the attachment flow.
- [x] Resolved `avatar_url` in user/room payloads (presigned URL, Gravatar fallback ADR-011/017).

## Attachments

- [x] Object storage abstraction (`service.Storage`; minio-go S3 impl + Disabled).
- [x] Upload sessions, presigned PUT/GET URLs, complete-verify (size/sha).
- [x] Metadata (filename sanitized, mime validated, sha256).
- [x] Authorization: uploader/admin/message-linked room membership.
- [ ] Image metadata extraction (width/height) server-side — fields exist, values are client-declared only.
- [ ] Object-store janitor for soft-deleted attachments.

## Realtime

- [x] WebSocket authentication (cookie or bearer; Hijacker-safe middleware).
- [x] Room subscriptions (membership-checked), typed event envelope.
- [x] Delivery acknowledgements recorded as receipts.
- [x] Reconnect/resync through REST (server design; client duty).
- [x] Heartbeats (30s pings), presence (connect/leave), typing relay (ephemeral).
- [x] Contact events, pin events, receipts-changed events.
- [ ] PostgreSQL LISTEN/NOTIFY for multi-instance (only when deployed multi-process). Note: also gates distributed presence broadcast and cross-instance push suppression (`SubscribedToRoom` is per-process).

## Android push

- [x] Device registration API (`/devices`, three-field push contract).
- [x] Android integration contract (docs/UNIFIEDPUSH.md).
- [x] Push endpoint persistence (validated flag, renewal, revocation).
- [x] Web Push encryption/delivery code (webpush-go, aes128gcm/RFC 8291, VAPID 8292, SSRF guard).
- [x] Integration test against the live self-hosted ntfy (`internal/push/live_test.go` + `TestLivePush*`): RFC 8291 roundtrip, validation ping §3.3, message push delivery — 2026-10-06.
- [x] Notification preferences wired end-to-end (`/users/me/notifications` → push policy → live delivery tested).
- [ ] Android distributor on-device test (ntfy Android app + our connector path) — needs a device, not a server concern.

## API

- [x] OpenAPI contract synchronized with the implementation (`api/openapi.yaml`; enforced by `scripts/contract-check.py`, runs under `go test ./scripts/`).
- [x] Error model (`{error:{code,message}}`) with logged 500s.
- [x] Rate limits (per-visitor token bucket).
- [ ] Rate-limit behavior tests (429 + `Retry-After`).
- [x] Request IDs.
- [x] Audit logging (auth, user admin, room admin, attachments).
- [ ] Idempotency keys for retried creates.

## Definition of done

- [x] `go test ./...` passes (unit + integration when `TEST_DATABASE_URL` set).
- [x] Static analysis passes (go vet, golangci-lint).
- [x] Database migrations are repeatable (goose; rerunnable suite).
- [x] OpenAPI validates (parse + `$ref`s + route parity via `scripts/contract-check.py`).
- [x] Authentication is tested (cookie + bearer + boundaries).
- [x] Room authorization is tested (private room existence + member-only access).
- [x] Message pagination is tested.
- [x] Receipt recording and visibility are tested (ADR-009 + preference gating).
- [x] Pinning is tested (member 403 / admin 204).
- [x] Contacts and user search are tested.
- [x] WebSocket reconnect behavior is supported via REST resync; live-test for resync flow pending.
- [x] Attachment authorization is tested.
- [x] Preferences behavior (including last-seen/read visibility) is tested.
- [x] Push registration and notification flow are tested against live ntfy (server side; on-device Android test remains).
- [x] CLI works without manually editing the database.
- [x] No secrets committed (verify before each commit).
- [x] Server can run as one Linux binary.