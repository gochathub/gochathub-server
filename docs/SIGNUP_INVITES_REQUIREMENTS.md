# Email signup invites — requirements

Status: discovery complete, not designed or implemented. Supersedes the "no self-registration / no email" stance of ADR-014 (a new ADR is required).

## Goal

An admin invites a person by email. The email carries a URL to a signup form that is reachable only through that invite. The invitee supplies profile information and the server creates their account and signs them in.

## Decisions (from discovery)

| Topic | Decision |
|---|---|
| Who invites | Admins only |
| Invite payload | Email only; resulting role is always `user` |
| Token | 7-day expiry, single-use, revocable, stored hashed; 256-bit random |
| Resend | Resend rotates the token and resets expiry |
| Duplicates | Existing account for the email → 409. Open invite for the email → treated as resend |
| Signup fields | username, display name, password, timezone, accent color (`primary_color`). Email pre-filled from the invite and read-only |
| After submit | Account created and session cookie issued in one call; client lands in chat |
| Abuse guard | Turnstile on submit (when configured) plus per-IP rate limit on both public endpoints |
| Email transport | SMTP, configured by admins in the webui (stored in the DB, password encrypted with env `SECRETS_KEY`); no SMTP env vars. Feature is off until configured. Postmark API later |
| Admin surfaces | CLI + REST + webui admin page (list / create / revoke / resend) |

## Functional requirements

1. FR1 Admin creates an invite for an email address; the server emails a link `ORIGIN/<signup-path>?token=…`.
2. FR2 Admin lists invites with status (open, used, expired, revoked), revokes, and resends.
3. FR3 A public lookup endpoint validates a token and returns only the invited email. Invalid, expired, used and revoked tokens all return the same generic error.
4. FR4 A public submit endpoint consumes the token and creates the user in one transaction. A concurrent double-submit must create at most one account.
5. FR5 Submit validates username (`[a-z0-9_.-]{1,64}`), display name, password via the existing policy, timezone (IANA), and accent color (one of the 15 swatches). Username conflict returns a recoverable error without consuming the token.
6. FR6 Submit sets the session cookie, as the normal login does.
7. FR7 Audit-log invite create, resend, revoke and signup completion. Never log the token.
8. FR8 CLI parity (`invite create|list|revoke|resend`) through the same service layer.
9. FR9 Webui: public signup route reachable only with a valid token (no link from the sign-in page); admin page visible to admins only.

## Non-functional requirements

- Feature is disabled and its routes return 404 when no sender is configured (same pattern as `RC_PAT_KEY`).
- Token is never stored in plaintext, never logged, and is sent only in the emailed URL. Put it in the URL fragment or POST body where practical to keep it out of access logs and referrers. Set `Referrer-Policy: no-referrer` on the signup page.
- Email sending is asynchronous or time-bounded. A provider outage must not hang the admin request, and a send failure must surface to the admin.
- Sender is an interface with a fake for tests, which is a genuine boundary.
- OpenAPI updated first (contract-check parity); webui schema regenerated.
- Email address stored lowercase, same rule as `CreateUser`.

## Naming

`/invites` already means room invites. Use a distinct name (`signup-invites` or `registrations`) across API, CLI, tables and webui.

## Acceptance criteria

- Valid link → form shows the locked email; submit creates the account, signs in, lands in chat.
- Reusing, expiring or revoking a link blocks signup with a generic error.
- Resend invalidates the previous link.
- A non-admin cannot create, list, revoke or resend (authorization-boundary tests).
- Two concurrent submits with one token → exactly one user.
- With no sender configured, endpoints are absent and the admin UI hides the page.
- The `go test ./...` integration suite covers all of the above using the fake sender.

## Resolved questions

1. Webui admin area: none exists (only group-room roles). This feature creates the first admin-only page.
2. Email: plain text, names the inviting admin, From = configurable `MAIL_FROM`.
3. CLI `user create` for an email with an open invite wins and auto-revokes the invite in the same transaction.
4. Password: min 12 chars, max 128, no composition rules. `internal/pwd` has no policy today, so signup adds the check; other paths unchanged.
5. First sender: SMTP. Postmark HTTP API later, behind the same interface.
6. Rate limits: reuse the existing login limiter values.

## Open questions

None.

## Next step

`/sc:design` for schema (migration 008), API contract, sender interface and webui route/store; then an ADR superseding ADR-014.
