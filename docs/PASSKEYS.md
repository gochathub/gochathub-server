# Passkeys — design

Status: server side implemented (ADR-025); web UI and Android pending.

Deviations from the sketch below, as built: `user_passkeys` stores the library Credential as one `credential jsonb` column (no per-field columns); CLI is `user passkey-list` / `user passkey-reset` (flat, like `2fa-reset`); `begin` endpoints are not in the strict rate bucket, `finish` and `/auth/mobile/token` share `passkeyRate`; the list/rename/delete service methods are plain `AuthService` methods. Migration is `db/migrations/007_passkeys.sql`.

## Decisions (from requirements)

| Topic | Decision |
|---|---|
| Role | Passwordless alternative. A passkey login is full MFA: no TOTP prompt. Password (+TOTP) login unchanged. |
| Library | `github.com/go-webauthn/webauthn` (v0.17.x). `BeginDiscoverableLogin` / `FinishPasskeyLogin`. |
| RP | RP ID = host of `ORIGIN`; allowed origin = `ORIGIN`. `ORIGIN` unset → feature off. Dev: `https://gochat.premadev.com` (dev passkeys do not work on prod). No new env vars. |
| Registration | Session principal only (not API tokens). Inline re-auth: password, plus TOTP/backup code when TOTP is enabled (same shape as `DisableTOTP`). No server-side "recently verified" state. |
| Attestation | `none`. Resident key required, user verification required. |
| Android | Browser handoff (Custom Tab → web login → PKCE-style one-time code → deep link → token). No Credential Manager, no `assetlinks.json`. |
| Last-passkey delete | Refused (`409`) when the user has no usable password (`password_hash` empty or `'!'`). Dead guard today; cheap, kept by decision. |
| User handle | The user's UUID bytes. Stable, not PII, no new column (avoids `userCols` churn). |

## Schema — `db/migrations/007_passkeys.sql`

```sql
CREATE TABLE user_passkeys (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id   bytea NOT NULL UNIQUE,
    public_key      bytea NOT NULL,
    sign_count      bigint NOT NULL DEFAULT 0,
    transports      text[] NOT NULL DEFAULT '{}',
    aaguid          bytea,
    backup_eligible boolean NOT NULL,
    backup_state    boolean NOT NULL,
    name            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz
);
CREATE INDEX user_passkeys_user ON user_passkeys (user_id);

-- Single-use WebAuthn ceremony state (go-webauthn SessionData). Postgres, not
-- process memory: survives restarts, safe with >1 instance.
CREATE TABLE webauthn_ceremonies (
    token_hash   bytea PRIMARY KEY,
    kind         text NOT NULL CHECK (kind IN ('register','login')),
    user_id      uuid REFERENCES users(id) ON DELETE CASCADE, -- null for discoverable login
    session_data jsonb NOT NULL,
    expires_at   timestamptz NOT NULL
);

-- One-time mobile handoff code, bound to the app's PKCE challenge.
CREATE TABLE mobile_handoffs (
    code_hash      bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_challenge text NOT NULL,
    expires_at     timestamptz NOT NULL
);
-- +goose Down: drop all three.
```

Ceremony/handoff rows are consumed with `DELETE ... WHERE token_hash=$1 AND expires_at>now() RETURNING ...` (atomic, single use). Expired rows are deleted opportunistically on insert; no sweeper.

## API (OpenAPI first; add `tags: [passkeys]`)

| Route | Auth | Body → Response |
|---|---|---|
| `POST /auth/passkey/login/begin` | none, `loginRate` | `{}` → `{ceremony_id, options}` |
| `POST /auth/passkey/login/finish` | none, `loginRate` | `{ceremony_id, credential, token_request?}` → `AuthResponse` (same as `/auth/login`) |
| `POST /users/me/passkeys/begin` | session | `{password, code?}` → `{ceremony_id, options}` |
| `POST /users/me/passkeys/finish` | session | `{ceremony_id, name, credential}` → `Passkey` |
| `GET /users/me/passkeys` | session | → `[Passkey]` |
| `PATCH /users/me/passkeys/{id}` | session | `{name}` → `Passkey` |
| `DELETE /users/me/passkeys/{id}` | session | → 200; `409` last-passkey guard |
| `POST /auth/mobile/code` | session | `{code_challenge}` → `{code}` (TTL 60 s) |
| `POST /auth/mobile/token` | none, `loginRate` | `{code, code_verifier}` → `{token}` (normal API token, name `mobile`) |

- `options` / `credential` are the WebAuthn JSON forms (`PublicKeyCredentialCreationOptionsJSON`, `…RequestOptionsJSON`, `RegistrationResponseJSON`, `AuthenticationResponseJSON`). Declare as opaque `object` in OpenAPI; the contract documents them by reference to the W3C JSON types.
- `Passkey`: `{id, name, created_at, last_used_at, backup_eligible, transports}`. Never expose credential id / key.
- Passkey endpoints return `404 passkeys_disabled` when the feature is off.
- `/auth/mobile/code` and `/auth/mobile/token` are method-agnostic: any logged-in web session (password, TOTP or passkey) can hand off.
- No Turnstile on passkey login (phishing-resistant, no password to brute-force); rate limit only.

## Service (`internal/service/passkeys.go`, `AuthService` methods; CLI shares them)

```
BeginPasskeyRegistration(ctx, p, password, code) (ceremonyID string, opts *protocol.CredentialCreation, err)
FinishPasskeyRegistration(ctx, p, ceremonyID, name, r *http.Request) (store.PasskeyRow, error)
BeginPasskeyLogin(ctx) (ceremonyID, *protocol.CredentialAssertion, error)
FinishPasskeyLogin(ctx, ceremonyID, r, userAgent, ip) (token string, store.UserRow, error)
ListPasskeys / RenamePasskey / DeletePasskey(ctx, p, id)
RemovePasskeys(ctx, username)              // CLI: delete all + revoke sessions
CreateMobileCode(ctx, p, challenge) / ExchangeMobileCode(ctx, code, verifier)
```

- A `webauthnUser` adapter implements `webauthn.User` over `UserRow` + `[]PasskeyRow`.
- `FinishPasskeyLogin` flow: consume ceremony → library lookup callback loads user by handle → reject `!Enabled` and shadow accounts → library verifies (UV, origin, RP ID hash, signature) → reject on `CloneWarning` (audit `auth.passkey_clone_warning`) → persist `sign_count`, `backup_state`, `last_used_at` → `issueSession`. Every failure returns `ErrUnauthorized` (no unknown-credential vs bad-assertion distinction); failed attempts audited like `auditLogin(false)`.
- Registration re-auth: verify password with `pwd.Verify`; if `TOTPByUser` is enabled, require `checkSecondFactor`. Registration excludes existing credential ids (`WithExclusions`). Shadow accounts (`password_hash='!'`) refused at `Begin`. Cap 10 passkeys per user.
- `ExchangeMobileCode`: `base64url(sha256(code_verifier)) == code_challenge` (constant-time), then `issueAPIToken`. Both failure modes → `ErrUnauthorized`.
- Wiring: build `*webauthn.WebAuthn` in `internal/app` only when `ORIGIN` is set; follow the `svc.RC` nil-interface rule (assign only when non-nil).
- Audit: `passkey.register`, `passkey.rename`, `passkey.remove`, `auth.login` (detail `{"method":"passkey"}`), `auth.login_failed`, `mobile.handoff`.

## Store (`internal/store/passkeys.go`)

`CreatePasskey`, `PasskeysByUser`, `PasskeyByCredentialID`, `UpdatePasskeyUsage` (sign_count, backup_state, last_used_at), `RenamePasskey(userID,id,name)`, `DeletePasskey(userID,id)` (owner-scoped → `ErrNotFound` for others), `DeleteUserPasskeys`, `CountPasskeys`, `CreateCeremony`/`TakeCeremony`, `CreateHandoff`/`TakeHandoff`. All via `Q`; ceremony/handoff writes inside `WithTx` where multi-step.

## CLI

`chat-server user passkey list <username>` and `chat-server user passkey remove <username> [--all]` — thin wrappers over `RemovePasskeys`; `--all` also revokes sessions (lost-device path, mirrors `2fa-reset`). Document in `docs/CLI.md`.

## Sequences

Login (web):
```
web  → POST /auth/passkey/login/begin
srv  → BeginDiscoverableLogin; store ceremony; {ceremony_id, options}
web  → navigator.credentials.get(options)   (conditional UI on username field optional)
web  → POST /auth/passkey/login/finish {ceremony_id, credential}
srv  → take ceremony; FinishPasskeyLogin; issueSession; Set-Cookie; AuthResponse
```

Android handoff:
```
app  → gen state + code_verifier; challenge = b64url(sha256(verifier))
app  → Custom Tab: {server}/login?mobile=1&challenge=…&state=…
web  → user signs in (passkey, or password/TOTP); then POST /auth/mobile/code {code_challenge}
web  → location = gochathub://login?server=…&code=…&state=…
app  → verify state; POST /auth/mobile/token {code, code_verifier} → {token}; store as today
```
The existing `gochathub://login?server=&token=` QR link stays supported.

## Clients

Web (`../gochatwebui`):
- Contract: copy `api/openapi.yaml`, `bun run generate-schema`, `bunx prettier --write src/api/schema.d.ts`.
- Use native `PublicKeyCredential.parseCreationOptionsFromJSON` / `parseRequestOptionsFromJSON` and `credential.toJSON()` (no dependency). Fallback to `@simplewebauthn/browser` only if the support matrix hurts.
- Login page: "Sign in with passkey" button; hidden when `PublicKeyCredential` is missing or the server reports the feature off (verify how the UI learns `turnstile` config today and reuse that channel).
- Profile: Passkeys list (name, created, last used, rename, delete), "Add passkey" dialog with password (+ code) re-auth.
- `/login?mobile=1`: after any successful login (or existing session), call `/auth/mobile/code`, then redirect to the deep link. Replaces nothing in `mobileSignIn.ts`.

Android (`../gochatandroid`):
- `LoginScreen.kt`: "Sign in with browser / passkey" → Custom Tab (`androidx.browser`).
- `Auth.kt` / `chatuikit-core/.../hub/MobileSignIn.kt`: accept `code` (+`state`) on the existing `gochathub://login` filter, exchange via `/auth/mobile/token`, then continue as the token path does. Keep `token` param.
- Persist `state` + `code_verifier` across process death (needed if the OS kills the app while the tab is open).

## Tests

Server (`descope/virtualwebauthn` for full ceremonies; `TEST_DATABASE_URL`):
- Register → login → session issued, no TOTP prompt even with TOTP enabled.
- Register requires correct password; requires valid TOTP when enabled; API-token principal refused.
- Authorization boundaries: user B cannot list/rename/delete user A's passkey.
- Challenge replay, expiry, wrong ceremony kind, wrong origin, wrong RP ID all rejected.
- Disabled user and shadow account (`'!'`) cannot log in / register.
- Sign-count regression / clone warning rejected.
- Last-passkey delete guard (fake passwordless user via direct SQL).
- Feature off when `ORIGIN` unset (`404 passkeys_disabled`).
- Mobile handoff: wrong verifier, reused code, expired code, unknown code → 401; success mints a working API token.
- `scripts/contract-check.py` parity (runs under `go test ./scripts/`).

## Implementation order (one pass, no gates)

1. Migration 007, store, service, wiring, OpenAPI + routes/handlers, server tests.
2. CLI + docs (`API.md`, `CLI.md`, ADR-025, `IMPLEMENTATION.md` checklist).
3. Web UI (login, profile, `mobile=1`).
4. Android handoff.
Deploy: server first (`scripts/deploy.sh`, migration runs there), then webui, then app.

## Risks / notes

- Custom-scheme deep links can be intercepted by other apps; the PKCE challenge makes an intercepted `code` useless. `state` stops code injection.
- Changing `ORIGIN`/host invalidates all registered passkeys (RP ID change). Document in the ADR.
- `descope/virtualwebauthn` and go-webauthn are new dependencies; both justified (hand-rolling CBOR/COSE verification is a security risk).
- Open verification at implement time: exact go-webauthn v0.17 API names (`FinishPasskeyLogin` returns user + credential; confirm the `RPOrigins` config field and the JSON-form parsing helper) via Context7 before coding.
