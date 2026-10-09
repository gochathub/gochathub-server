# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.0.0] - 2026-10-09

First tagged release. GoChatHub is a self-hosted chat server: REST/OpenAPI + WebSocket, PostgreSQL as the authority, separate web and Android clients, UnifiedPush notifications via a self-hosted ntfy server, S3-compatible attachments. Single deployable Go binary with CLI administration.

### Added

- **Rooms and messaging**: rooms with member roles (owner/admin/member + PATCH role updates), invitations, soft-delete/archival (`room.archived` events), delivered/read receipts with delivery-accurate timing, batched message paging, message body search (`q` param), soft-deleted attachments, real-time delivery over WebSocket with REST resynchronization.
- **Auth and identity**: password login, TOTP two-factor auth (enrollment, backup codes, recovery reset), two-step login challenge, Cloudflare Turnstile gate on password login, self-service API tokens (mint, list, revoke), account sessions with revocation.
- **Push notifications**: UnifiedPush device registration/renewal/validation, ntfy fan-out with minimal (identifier/type-only) payloads, Web Push for the web client, push renewal fixes for re-sent endpoints.
- **Inbound webhooks**: Postmark and Cloudflare Email Worker mail-in via `POST /hooks/{id}/{secret}`; bot-user posting to a fixed room or DM, secret hashing, CIDR allowlist, attachment pipeline, CLI management (`webhook create/list/enable/disable/rotate/delete`).
- **Rocket.Chat migration**: `rcmigrate` importer with delta re-runs; TOTP secret and backup-code import for legacy users.
- **Attachments**: S3-compatible storage with presigned URLs, `S3_PUBLIC_ENDPOINT` for upstream-TLS deployments, path-traversal and SSRF guards, per-object authorization.
- **Deployment**: Dockerfile (multi-stage, distroless nonroot), goose migrations, CLI administration sharing the service layer with HTTP handlers, OpenAPI contract with `scripts/contract-check.py`.
- **Preferences**: spellcheck preference storage (JSONB, no migration).

### Fixed

- Push renewal 500 on re-sent endpoints (global-unique endpoint row dropped first).
- WS token revocation, attachment sha256 checks, invite expiry, config hardening findings from live security smoke.
- Invite-path defects found by live security smoke.
- Preferences decode no longer resets all prefs to defaults on unknown keys.

### Security

- Login rate limiting, request size limits, rich-text sanitization, session/token handling, audit logging, room- and attachment-level authorization tests. Internet-facing posture; see `README.md` Security section.

## [Unreleased]

Changes since 1.0.0 land here as they accumulate.