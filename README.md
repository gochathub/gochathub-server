<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/gochathub-wordmark-dark.png">
    <img src="assets/gochathub-wordmark.png" alt="goChatHub" height="128">
  </picture>

  # gochathub-server

  **Self-hosted chat backend in Go — one binary, PostgreSQL, no middleware stack.**

  [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
  [![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](go.mod)
  [![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16%2B-4169E1?logo=postgresql&logoColor=white)](docs/DATABASE.md)

</div>

`gochathub-server` is the backend of [goChatHub](https://github.com/gochathub) — a chat server that ships as a single deployable Go binary. REST/OpenAPI contract, WebSocket fan-out, S3-compatible attachments, push notifications through your own ntfy server, and a CLI for administration. It speaks to any client; the official web and Android clients are maintained separately.

No PostgREST, Redis, Kafka, RabbitMQ, or microservices. PostgreSQL is authoritative; WebSocket is only a delivery mechanism, so clients reconnect and resynchronize over REST.

## Features

- **Sessions and tokens** — browser session cookies plus bearer API tokens; passwords hashed with Argon2id.
- **Rooms** — public, private, direct, and group-direct rooms with membership, invitations, pins, and archived state.
- **Receipts, typing, presence** — delivery and read receipts pushed over the WebSocket.
- **Contacts and privacy preferences** — per-user defaults for who can find or message you.
- **Attachments** — presigned S3 upload sessions; the backend proxies authorization, never file bytes.
- **Push notifications** — UnifiedPush (self-hosted ntfy) and Web Push (RFC 8291 encrypted payloads); only identifiers go out, never message content.
- **Avatars** — uploaded through attachments with Gravatar fallback.
- **Admin CLI** — users, rooms, and tokens managed from the same service layer as the HTTP API.

## Architecture

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

## Quick start

Prerequisites: Go 1.26+, PostgreSQL 16+, and (optional) any S3-compatible object store and ntfy server.

```bash
go build -o gochathub-server ./cmd/chatserver
gochathub-server migrate      # apply migrations first; startup never migrates implicitly
gochathub-server user create alice
gochathub-server serve
```

The server is now listening on `:8080`. The OpenAPI contract lives in [`api/openapi.yaml`](api/openapi.yaml).

## CLI

```bash
gochathub-server user create <username>          # and list | passwd | enable | disable | delete
gochathub-server room create <name>              # and list | archive | members | invite
gochathub-server token create <username>         # raw token printed once; and revoke
```

See [`docs/CLI.md`](docs/CLI.md) for the full reference.

## Configuration

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
| `TURNSTILE_SECRET` / `TURNSTILE_HOSTNAME` | (empty) | Cloudflare Turnstile on `POST /auth/login`; unset = off. Hostname optionally pins the widget's site |
| `S3_REGION` / `S3_USE_TLS` | `us-east-1` / `true` | S3 connection |
| `S3_PUBLIC_ENDPOINT` | (empty) | Browser-visible URL (`https://host`) presigned links are signed for; `S3_ENDPOINT` stays the address this server uses. Empty = same as `S3_ENDPOINT` |
| `MAX_UPLOAD_BYTES` | `26214400` | Per-attachment limit |
| `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` | (empty) | Generated + persisted in DB on first boot when unset |
| `VAPID_SUBSCRIBER` | `https://chatserver.invalid` | VAPID `sub` claim |
| `PUSH_NTFY_QUERY` | `up` | Appends `?up=1` to push sends (ntfy UnifiedPush flag) |
| `PUSH_ALLOW_HOSTS` | (empty) | Comma list of push hosts exempt from SSRF rejection (self-hosted ntfy) |
| `MIGRATIONS_ON_SERVE` | unset | Opt in to applying migrations on serve |

## Clients

- Web: [github.com/gochathub/gochathub-webui](https://github.com/gochathub/gochathub-webui) (Avian fork)
- Android: [github.com/gochathub/gochathub-android-client](https://github.com/gochathub/gochathub-android-client) (CometChat UI Kit fork, UnifiedPush)

The server is the contract authority for both.

## Documentation

| Document | Contents |
| --- | --- |
| [`api/openapi.yaml`](api/openapi.yaml) | Normative API contract |
| [`docs/API.md`](docs/API.md) | API notes |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | System architecture |
| [`docs/DATABASE.md`](docs/DATABASE.md) | Schema and migrations |
| [`docs/WEBSOCKETS.md`](docs/WEBSOCKETS.md) | WebSocket protocol |
| [`docs/CLI.md`](docs/CLI.md) | Admin CLI reference |
| [`docs/PUSH.md`](docs/PUSH.md) / [`docs/UNIFIEDPUSH.md`](docs/UNIFIEDPUSH.md) | Push design and the verified UnifiedPush contract |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | Architecture decision records (ADR-001…) |
| [`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md) | Feature checklist and out-of-scope list |

Status: the core server is implemented in one continuous pass. Not yet done: on-device Android distributor test (needs a device), image dimension extraction, idempotency keys, attachment janitor, and rate-limit tests.

## Development

```bash
go vet ./...
golangci-lint run ./...
go test ./...
TEST_DATABASE_URL="postgres://user@host/db?sslmode=disable" go test ./...   # integration suite
python3 scripts/contract-check.py   # api/openapi.yaml vs routes sync (also runs under go test ./scripts/)
```

The integration suite is rerunnable and applies migrations to the test database itself. `scripts/contract-check.py` fails on any drift between the OpenAPI contract and the mounted routes — keep parity on every route or schema change.

## Security

The server is designed to be Internet-facing: Argon2id password hashing, hashed token storage, per-visitor rate limiting, strict attachment authorization with presigned uploads, push-URL SSRF rejection, request size limits, rich-text sanitization, and audit logging. Please report vulnerabilities privately via [GitHub security advisories](https://github.com/gochathub/gochathub-server/security/advisories/new) rather than public issues.

## Contributing

PRs welcome. Keep the OpenAPI contract in sync (`python3 scripts/contract-check.py`) and attach tests for any authorization change.

## License

[MIT](LICENSE) © Brian Tafoya