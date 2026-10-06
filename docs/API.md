# API Contract

The normative API contract is `api/openapi.yaml`. Generated types
(oapi-codegen) come from that file. This document keeps only principles — do
not restate endpoint lists here; expand the contract alongside the
implementation instead. `scripts/contract-check.py` (also run under
`go test ./scripts/`) fails when the contract and the mounted routes drift
in either direction, or when a `$ref` stops resolving.

## Authentication

- Browser clients: opaque session cookie (`__Host-` prefix, Secure, SameSite=Lax, HttpOnly) with same-origin checks — ADR-015. The same cookie authorizes the WebSocket upgrade.
- Android, CLI, integrations: `Authorization: Bearer <token>` with revocable server-side API tokens.
- Sessions are opaque and revocable. No JWTs.
- Health endpoints (`/healthz`, `/readyz`, `/version`) require no authentication.

## Principles

- JSON; RFC 3339 timestamps.
- Stable opaque string IDs.
- Consistent error envelope with stable machine-readable codes.
- Request IDs.
- Cursor pagination for message history — never offset pagination.
- Idempotency keys for operations where retries create duplicates.
- Explicit versioning (`/api/v1`), OpenAPI is source of truth.
- Message payloads carry receipt state per ADR-009 visibility rules.