# Email signup invites — design

Inputs: `SIGNUP_INVITES_REQUIREMENTS.md`. Name used everywhere: **signup invites** (`/invites` already means room invites). Supersedes ADR-014 → add **ADR-026**.

## Flow

```
admin ──POST /admin/signup-invites {email}──▶ service.SignupInvites.Create
          │ insert row (token_hash), audit, Mailer.Send(link)
          ▼
invitee mail: ORIGIN/signup#token=<256-bit>      (fragment: never reaches server logs or Referer)
          │
SPA /signup ──POST /signup/lookup {token}──▶ {email}            (generic 404 on any failure)
          │
SPA submit ──POST /signup {token, username, display_name, password, timezone, primary_color, turnstile_token}
          ▼
   tx: consume invite (UPDATE … WHERE unused AND unrevoked AND unexpired RETURNING email)
       insert user (role=user, email from invite, prefs.primary_color, timezone)
       commit → issueSession → Set-Cookie → 200 {user}
```

Username/validation failures roll the tx back, so the token stays usable. The consume `UPDATE` row-locks, so concurrent submits yield exactly one account.

## Database — migration `008_signup_invites.sql`

```sql
-- +goose Up
CREATE TABLE signup_invites (
    id           uuid PRIMARY KEY,
    email        text NOT NULL CHECK (email = lower(email) AND position('@' in email) > 1),
    token_hash   bytea NOT NULL UNIQUE,
    invited_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_sent_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    used_by      uuid REFERENCES users(id) ON DELETE SET NULL,
    revoked_at   timestamptz
);
-- at most one open invite per email
CREATE UNIQUE INDEX signup_invites_open_email
    ON signup_invites (email) WHERE used_at IS NULL AND revoked_at IS NULL;
-- +goose Down
DROP TABLE signup_invites;
```

Status is derived: `used` > `revoked` > `expired` (`expires_at <= now()`) > `open`. Resend = `UPDATE token_hash, expires_at = now()+7d, last_sent_at`. Expired-but-unrevoked rows still occupy the unique index; Create on such an email acts as resend.

## Service layer (`internal/service/signup_invites.go`)

```go
type Mailer interface { Send(ctx context.Context, m Mail) error } // only real boundary: fake in tests
type Mail struct{ To, Subject, Body string } // plain text

type SignupInviteService struct{ App *App; Mail Mailer /* nil = disabled */; Turnstile func(...) ; TTL time.Duration }

Create(ctx, p Principal, email string) (SignupInvite, error)   // admin only; 409 if users.email exists; open invite → resend path
List(ctx, p) ([]SignupInvite, error)
Resend(ctx, p, id) error
Revoke(ctx, p, id) error                                       // only open invites
Lookup(ctx, token) (email string, err error)                   // ErrNotFound for every failure mode
Complete(ctx, in CompleteInput, ua string, ip *string) (token string, store.UserRow, error)
```

- Admin check: `p.IsAdmin()` else `ErrForbidden` (same pattern as existing services).
- `Disabled` (Mail == nil): handlers and CLI return `ErrNotFound`/"email not configured".
- Send failure after insert: row is kept, Create returns `ErrMailFailed` → HTTP 502 `mail_failed`; admin retries with Resend. Send has a 10 s timeout.
- `Complete`: password check `12 ≤ len ≤ 128` (new `checkSignupPassword`, signup-only; the 128 cap also bounds argon2 cost), username via `usernameRe`, timezone via `time.LoadLocation`, `primary_color` via the existing swatch validator (reuse from preferences; no copy). User row built through the same constructor `CreateUser` uses (extract `newUserRow` so validation lives once). Session via `AuthService.issueSession`.
- **CLI wins**: `CreateUser` runs in `WithTx` and revokes any open invite for its email.
- Email stored lowercase/trimmed; same rule as `CreateUser`.

## Mail transport (`internal/mail`, stdlib `net/smtp`)

`SMTP` implements `Mailer`. Env (all operator-supplied, no user-controlled host → no SSRF surface):

| Var | Meaning |
|---|---|
| `SMTP_HOST` | feature switch; unset = off |
| `SMTP_PORT` | default 587 |
| `SMTP_USER` / `SMTP_PASS` | optional auth (PLAIN, only over TLS) |
| `SMTP_TLS` | `starttls` (default), `implicit`, `none` (dev only) |
| `MAIL_FROM` | required when `SMTP_HOST` set |

Startup fails if `SMTP_HOST` is set without `MAIL_FROM` or `ORIGIN` (the link base). Header injection: reject CR/LF in To/Subject/From. A Postmark HTTP `Mailer` is a later drop-in; not built now.

Body (plain text):

```
<inviter display name> invited you to goChatHub.

Create your account (link valid 7 days):
<ORIGIN>/signup#token=<token>

If you weren't expecting this, ignore this email.
```

## HTTP API (OpenAPI first; `scripts/contract-check.py` parity)

| Route | Auth | Notes |
|---|---|---|
| `GET /api/v1/admin/signup-invites` | admin session | list `[SignupInvite]`; 404 when mail disabled |
| `POST /api/v1/admin/signup-invites` | admin | `{email}` → 201 `SignupInvite`; 409 `conflict` existing user; 502 `mail_failed` |
| `POST /api/v1/admin/signup-invites/{id}/resend` | admin | rotates token |
| `DELETE /api/v1/admin/signup-invites/{id}` | admin | revoke |
| `POST /api/v1/signup/lookup` | none | `{token}` → `{email}`; generic 404 `not_found` |
| `POST /api/v1/signup` | none (+Turnstile) | → 200 `{user}` + session cookie; 409 `username_taken`; 400 `validation`; 404 `not_found` |

`SignupInvite`: `id, email, status(open|used|revoked|expired), invited_by, created_at, last_sent_at, expires_at, used_at`. Never includes the token.

Public routes: `a.chain(false, …)`, `limitBody` small cap, `loginRate.allow(visitorIP)` first (shared login limiter), Turnstile via `a.svc.Auth.VerifyCaptcha` on `POST /signup` only, before argon2. Token travels in the POST body, never the URL; ensure `logpath` redaction is unaffected.

Audit: `signup_invite.create|resend|revoke|complete` (target = invite id; detail never holds the token; complete also audits the new user id).

## CLI (`internal/cli`)

`chat-server signup-invite create <email> | list | revoke <id> | resend <id>` — calls the same service with a synthetic admin principal (follow how existing CLI commands act).

## Webui (`../gochatwebui`)

1. **Contract**: copy `api/openapi.yaml`, `bun run generate-schema`, `bunx prettier --write src/api/schema.d.ts`.
2. **Router** (`src/router.ts`):
   - `/signup` (name `Signup`): public; guard treats like `Help`/`Access` — no redirect for logged-out users, no auto-redirect needed for logged-in ones.
   - `/admin/:section?` (name `Admin`): top-level route (not under `HomeView`, so it fills the page); guard requires `auth.me?.role === "admin"`, else `No-Chat`.
3. **Left rail link** (`Navigation/Navigation.vue`): `NavLink` with `ShieldCheckIcon`, title "Admin", `v-if="auth.me?.role === 'admin'"`, `@click="router.push('/admin')"`. Place in the main nav list (and the small-screen layout). It navigates to a route, unlike the sidebar-switching links, so no `activeSidebarComponent` entry.
4. **Admin page** (`views/AdminView/AdminView.vue`): full-page layout with a section list on the left (initially one section: **Signup invites**) and a "Back to chat" link. Sections are components keyed by `:section`, so future admin options slot in without router changes.
5. **Signup invites section**: table (email, status badge, sent, expires), "Invite by email" form, per-row Resend / Revoke (open rows only). A 404 from the list renders "Email is not configured on this server."
6. **Signup view** (`views/SignupView/SignupView.vue`): reads `location.hash` token, immediately `history.replaceState` to strip it, calls lookup, shows locked email, fields username, display name, password (+ confirm, 12-char hint), timezone (default `Intl.DateTimeFormat().resolvedOptions().timeZone`, select from `Intl.supportedValuesOf("timeZone")`), accent color (the 15 swatches already used in settings — reuse the component), Turnstile widget (reuse `src/turnstile.ts`). Success → set `auth.me`, `router.replace({name:"No-Chat"})`. Invalid token → static "This invite link is invalid or has expired. Ask an admin for a new one." No link to sign-in's signup (none exists).
7. **Store** `src/store/signupInvites.ts` (Pinia) for admin list/create/resend/revoke; signup calls in `auth.ts` (`lookupInvite`, `signup`). Types from the generated schema only.
8. Update `docs/CLIENT_BRIEF.md`/`CLAUDE.md` "Remove: signup" note to say signup exists only via invite.

## Security checklist

- 256-bit token (`id.NewToken`), stored as `id.HashToken`; compared by hash lookup.
- Single response shape for invalid/expired/used/revoked tokens; lookup rate-limited.
- Fragment token + `history.replaceState` + `Referrer-Policy: no-referrer` on the SPA signup page (note for the Caddy edge config).
- Role is hardcoded `user` in `Complete`; no client-supplied role/email accepted (email comes from the invite row).
- Authorization-boundary tests: non-admin and anonymous → 403/401 on all four admin routes.
- Existing-account email: admin-only 409 (no enumeration to the public).
- Disabled feature: admin routes and both public routes return 404.

## Tests

Integration (`internal/httpapi`, Postgres, fake `Mailer` capturing the link):

1. Happy path: create → email captured → lookup → signup → cookie works on `/users/me`; prefs `primary_color` + timezone persisted; role `user`.
2. Reuse, expiry (time-shifted row), revoke, resend-invalidates-old-token → all 404 `not_found`.
3. Username conflict → 409 and token still valid afterwards.
4. 20 concurrent submits, one token → exactly one user.
5. Non-admin/anonymous on admin routes; mail-disabled → 404s.
6. Existing user email → 409; re-invite open email → resend (one row, new token).
7. CLI `CreateUser` for an invited email revokes the invite.
8. Weak password (<12, >128), bad timezone, bad color → 400.
9. Unit: SMTP header-injection rejection; `checkSignupPassword`.

Webui: Playwright/Vitest per existing `tests/` layout — signup happy path against mocked API, admin link visible only for admins.

## Implementation order (each step compiles and passes tests)

1. ADR-026 + OpenAPI (+ contract-check green).
2. Migration 008 + store queries.
3. `internal/mail` SMTP + config/env + `Mailer` fake.
4. `SignupInviteService` + `CreateUser` revoke hook + tests.
5. Handlers/routes + integration tests.
6. CLI.
7. Server deploy (new env vars into `/opt/stacks/gochathub/.env`, back up first).
8. Webui: schema, router, rail link, admin page, signup view, store, tests; then deploy `dist/`.

## Assumptions to confirm

- The SPA is served from the same origin as `ORIGIN`, so the invite link is `ORIGIN/signup`. If the edge routes differ, add a `SIGNUP_BASE_URL` override.
- An admin session is enough to manage invites (no extra TOTP re-auth).
