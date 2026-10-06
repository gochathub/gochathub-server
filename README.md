# gochathub-server — Go PostgreSQL Chat Server (Claude Code Project Brief)

The backend of **goChatHub** (github.com/gochathub): a clean, single-binary chat server.

## Goal

Build a production-quality chat backend using:

- Go
- PostgreSQL
- REST/OpenAPI
- WebSockets
- UnifiedPush for Android push notifications
- Existing self-hosted ntfy server as the UnifiedPush distributor
- S3-compatible object storage for attachments
- CLI administration
- Separate web and Android clients

The backend must be independent of either client and usable by existing custom applications.

## Core architectural principle

Keep the system deliberately small:

```text
                         +------------------+
                         |    Web Client    |
                         +--------+---------+
                                  |
                              REST/WSS
                                  |
+------------------+      +-------v--------+      +------------------+
| Existing/custom  |----->|   Go Server    |----->|   ntfy server    |
| applications     | API  |                |      | UnifiedPush      |
+------------------+      | REST/OpenAPI   |      +--------+---------+
                          | WebSocket      |               |
                          | Auth/ACL       |               v
                          | Chat           |         Android client
                          | Attachments    |
                          +-------+--------+
                                  |
                           +------v-------+
                           | PostgreSQL   |
                           +--------------+
```

Do NOT introduce PostgREST, Redis, Kafka, RabbitMQ, microservices, or a separate notification service unless a demonstrated requirement appears later.

## Repository expectation

Claude Code should turn this specification into the actual implementation. Do not merely create documentation. Start by inspecting the repository, then implement incrementally with tests.

## Suggested initial Go dependencies

Prefer small, mature libraries:

- pgx/v5
- sqlc
- chi
- oapi-codegen
- a maintained WebSocket implementation
- a migration tool such as goose or golang-migrate
- zerolog or slog for structured logging
- cobra for CLI

Use Go's standard library wherever practical.

## Runtime

Target Linux first. Produce one server binary and one admin CLI binary, or a single binary with subcommands if that is cleaner.

Example:

```bash
`gochathub-server serve`, `gochathub-server migrate`,
`gochathub-server user create`, `gochathub-server room create`.
```

A single binary with subcommands is preferred unless it materially complicates deployment.

## Important

The ntfy server already exists. Do not implement an ntfy server.

The Android application will use UnifiedPush. The chat backend should not implement FCM/APNs directly.

See docs/ARCHITECTURE.md, docs/DATABASE.md, docs/API.md, docs/WEBSOCKETS.md, and docs/PUSH.md.

Client repositories: github.com/gochathub/gochathub-webui (Avian fork) and
github.com/gochathub/gochathub-androidclient (CometChat UI Kit fork) — directed by
docs/CLIENT_WEB.md and docs/CLIENT_ANDROID.md respectively; the server is
the contract authority for both.

## Implementation status

Implemented as one continuous pass (see docs/IMPLEMENTATION.md for the checklist and out-of-scope list):

Single binary `gochathub-server` (API + WebSocket) with the admin subcommands:
- Auth: browser session cookie (ADR-015), bearer API tokens, Argon2id.
- Rooms (public/private/direct/group_direct), membership, invitations, pins, receipts (ADR-009), contacts (ADR-010), privacy preferences (ADR-013), avatars with Gravatar fallback (ADR-011).
- Attachments: presigned S3 upload sessions.
- WebSocket fan-out with receipts and typing/presence.
- UnifiedPush/Web Push sender per docs/UNIFIEDPUSH.md — verified live against
  the self-hosted ntfy (RFC 8291 roundtrip, validation ping, message push).
- CLI: migrate, serve, version, user, room, token.

Not yet: on-device Android distributor test (needs a device), image dimension extraction, idempotency keys, attachment janitor, rate-limit tests.

## Build and run

```bash
go build -o gochathub-server ./cmd/chatserver
gochathub-server migrate   # apply migrations first
gochathub-server version
gochathub-server serve
```

Runtime startup never migrates implicitly; opt in per process with `MIGRATIONS_ON_SERVE=1`.

## Configuration (environment)

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | (required) | PostgreSQL connection string |
| `LISTEN_ADDR` | `:8080` | HTTP listener |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `SESSION_TTL` | `720h` | Session cookie lifetime |
| `COOKIE_SECURE` | `true` | `false` only for local HTTP development |
| `ORIGIN` | (empty) | Deployment origin for same-origin checks (ADR-015) |
| `TRUST_PROXY` | `true` | Visitor IP from X-Forwarded-For (reverse proxy) |
| `RATE_LIMIT_RPM` | `60` | Per-visitor burst/refill for capped routes |
| `S3_ENDPOINT` `S3_BUCKET` `S3_ACCESS_KEY` `S3_SECRET_KEY` | (empty) | Object storage for attachments; without them uploads are rejected |
| `S3_REGION` / `S3_USE_TLS` | `us-east-1` / `true` | S3 connection |
| `MAX_UPLOAD_BYTES` | `26214400` | Per-attachment limit |
| `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` | (empty) | Generated + persisted in DB on first boot when unset |
| `VAPID_SUBSCRIBER` | `https://chatserver.invalid` | VAPID `sub` claim |
| `PUSH_NTFY_QUERY` | `up` | Appends `?up=1` to push sends (ntfy UnifiedPush flag) |
| `PUSH_ALLOW_HOSTS` | (empty) | Comma list of push hosts exempt from SSRF rejection (self-hosted ntfy) |
| `MIGRATIONS_ON_SERVE` | unset | Opt in to applying migrations on serve |

## Testing

```bash
go vet ./...
golangci-lint run ./...
go test ./...
TEST_DATABASE_URL="postgres://user@host/db?sslmode=disable" go test ./...   # integration suite
python3 scripts/contract-check.py   # api/openapi.yaml ↔ routes sync (also runs under go test ./scripts/)
```

The integration suite applies migrations to the test database; it is rerunnable. The contract checker fails when `api/openapi.yaml` and the mounted routes drift in either direction, and when any `$ref` in the contract stops resolving.
