# Inbound webhooks — design

Status: implemented (tracker: IMPLEMENTATION.md "Inbound webhooks"). Decisions: ADR-020..022. Sources: Postmark inbound webhook, Cloudflare Email Workers.

Goal: an external system (Postmark inbound, a Cloudflare Email Worker) POSTs one email-shaped JSON document and the server creates one message in a fixed room or DM.

## Decisions (from requirements discovery)

| Topic | Decision |
|---|---|
| Author | One bot user per webhook (`users.role = 'bot'`) |
| Recipient | Fixed at creation: one room, or one DM with one user. The payload never picks the target |
| Auth | Secret in path: `POST /hooks/{id}/{secret}`; stored hashed; optional CIDR allowlist |
| Notify | Normal push per recipient prefs; `@mentions` in webhook text never resolve |
| Attachments | Stored as bot-owned attachments via the existing attachment pipeline |
| Payload | Postmark inbound JSON (subset). The Cloudflare Worker emits the same shape |
| Dedupe | Postmark `MessageID` (Worker: the `Message-ID` header) |
| Management | CLI only (create/list/enable/disable/rotate/delete), same service layer as HTTP |

## Flow

```
Postmark / Worker --POST /hooks/{id}/{secret}--> httpapi.handleHook
   chain: recover > logging(redacted path) > request-id > sec headers > rate(IP)   [no origin check, no session auth]
   1 lookup webhook by id; constant-time compare sha256(secret); enabled; IP allowlist   -> else 403 (one body for all)
   2 try-acquire ingest semaphore                                                        -> else 503 + Retry-After
   3 MaxBytesReader(WEBHOOK_MAX_BODY_BYTES); decode InboundEmail                         -> 400 / 413
   4 service.Webhooks.Ingest(ctx, hookID, in)
        a spam gate (optional)            -> 200 {"dropped":"spam"}
        b dedupe lookup (source_id)       -> 200 {"duplicate":true}
        c principal = bot user (Kind "webhook")
        d attachments: for each (max 10): CreateUpload > Storage.PutObject > Complete   (failures become body lines)
        e compose body (see Rendering)
        f Messages.Create(bot, room, MessageInput{SkipMentions:true, AttachmentIDs})   [WS + push as usual]
        g insert webhook_deliveries; touch last_used_at
   5 200 {"message_id": "...", "duplicate": false}
```

Body is read only after step 1, so unauthenticated callers cannot make the server buffer large bodies.

## Data model (migration 004)

```sql
-- +goose Up
ALTER TYPE user_role ADD VALUE 'bot';   -- PG12+ allows this in a tx as long as the value is not used in the same migration

CREATE TABLE webhooks (
    id             uuid PRIMARY KEY,
    name           text NOT NULL,
    bot_user_id    uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE RESTRICT,
    room_id        uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    secret_hash    bytea NOT NULL,
    allowed_cidrs  text[] NOT NULL DEFAULT '{}',   -- empty = any source; parsed with net/netip in Go
    max_spam_score real,                            -- NULL = spam gate off
    enabled        boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    rotated_at     timestamptz,
    last_used_at   timestamptz
);

CREATE TABLE webhook_deliveries (
    webhook_id uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    source_id  text NOT NULL,
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, source_id)
);
```

- `room_id` is always a room. For a user target the bot-to-user DM room is created at webhook creation (reusing `GetDirectRoomBetween`), so ingest has a single code path.
- Bot user row: `username = 'bot-' || replace(webhook_id,'-','')`, `display_name = webhook name`, `email NULL`, `role 'bot'`, `password_hash = '!'` (never matches any hash). `Login` needs no special case: the sentinel makes `pwd.Verify` error, which login treats as a failed attempt (covered by `TestWebhookBotCannotLogin`). No sessions or API tokens are ever issued for a bot.
- `users` gains no column, so `userCols` and the Scan lists do not change. Clients see `role: "bot"` (add to the OpenAPI role enum).
- CLI `user create --role` already whitelists `user|moderator|admin` (`service/users.go:33`), so bots cannot be created that way.

## HTTP contract

`POST /hooks/{webhookId}/{secret}` (unversioned, like `/healthz`: the URL lives in third-party config; the payload shape is Postmark's).

Request: Postmark inbound JSON; unknown fields are ignored; `HtmlBody` is ignored and never stored.

```
MessageID, Subject, TextBody, StrippedTextReply, OriginalRecipient
FromFull{Email, Name}
Headers[{Name, Value}]                     -- only X-Spam-Score is read
Attachments[{Name, Content(base64), ContentType, ContentLength}]
```

Responses (Postmark retries on anything but 200 and stops on 403):

| Code | When |
|---|---|
| 200 | created, duplicate, or dropped as spam (body says which) |
| 400 | invalid JSON, or nothing to post |
| 403 | unknown id, bad secret, disabled, IP not allowed (identical body) |
| 404 | bot no longer a member of the room |
| 409 | room archived (Postmark retries for about 10 h, so an unarchive still delivers) |
| 413 | body over cap |
| 429 | IP rate limit (existing limiter) |
| 503 | ingest concurrency full (`Retry-After: 30`) |

## Rendering (the part that loses mail if wrong)

`markdown.Validate` rejects raw HTML tags and entities outside code, control characters, and bodies over 64 KiB. Email text routinely contains `<name@host>` (matches the tag regex) and `&amp;`. A naive body would be rejected and the mail lost. Rendering rules:

1. Header lines use inline code for every untrusted field (blocks link injection and tag matches):
   `**From:** ` + code(`Name <email>`) and `**Subject:** ` + code(subject). Newlines are removed and backticks replaced.
2. The text (`StrippedTextReply`, else `TextBody`) goes in a fenced code block. Backticks in the text are replaced with U+02CB (`ˋ`) because `stripCodeSegments` closes a fence at the first literal run of three backticks, which would expose the rest to the HTML check.
3. Control characters other than `\n \r \t` are dropped; text is truncated on a rune boundary to leave headroom under `MaxMessageBytes`, with a `[truncated]` marker.
4. Attachment problems add lines such as `skipped attachment report.pdf: too large`. More than 10 attachments: first 10 stored, rest listed.
5. Empty text and no attachments: body is `(no text content)`.

Trade-off: email renders monospaced, not as Markdown. That is the lowest-risk choice for hostile input. HTML-only mail (no `TextBody`) shows `(no text content)`; convert HTML to text later if that case matters.

`MessageInput` gains `SkipMentions bool` tagged `json:"-"`, so only the webhook service can set it. `Create` skips `mentionTargets` when set. Ordinary push still follows each recipient's notification prefs; only mention-priority is lost.

## Attachment ingestion

Reuse, don't duplicate: for each attachment the webhook service, acting as the bot principal, calls `Attachments.CreateUpload` (filename sanitizing, mime check, `MaxUploadBytes`, server-generated key; the presigned URL is ignored), then `Storage.PutObject` with the decoded bytes, then `Attachments.Complete` (size/sha verification). `checkAttachReadyOwned` already passes because the bot is the uploader.

- `Storage` interface gains `PutObject(ctx, key, mime, size, io.Reader)`; `S3` already has it, `Disabled` returns an error. Uploads disabled (`AllowUploads` false) means all attachments are listed in the body instead.
- Per-attachment failure never fails the request: it becomes a body line (ADR-022).
- A failed `Messages.Create` after uploads makes a best-effort `Attachments.Delete` of the uploaded ids. Remaining orphans fall under the existing abandoned-upload purge.

Memory ceiling: Postmark allows 35 MB of attachments, about 47 MB as base64 JSON, held roughly three times during decode. Mitigations: `WEBHOOK_MAX_BODY_BYTES` (default 64 MiB) and a 2-slot ingest semaphore, so worst case is two bodies in flight. Streaming decode is the upgrade path if that is ever too much.

## Dedupe

`webhook_deliveries(webhook_id, source_id)`: check before create, insert after. If the process dies between create and insert, a retry posts a duplicate message (at-least-once, never lost). A concurrent race hits the primary key; the loser soft-deletes its message. Empty `MessageID` means no dedupe. Rows are tiny and cascade with their message; no purge job.

## Security

- Secret: `id.NewToken()` generated at create/rotate, shown once, stored as `id.HashToken` (same helpers as API tokens), compared with `subtle.ConstantTimeCompare` after lookup by id.
- Log redaction is required. `withLogging` and `withRecover` log `r.URL.Path`. A shared `logPath(r)` helper must emit `/hooks/{id}/***` for the hook prefix. Reverse-proxy and CDN access logs are outside the app: configure them to drop or mask the path for `/hooks/`. This is the known weakness of path secrets; a header secret is the upgrade for sources that can set headers (the Worker can; Postmark cannot).
- CIDR allowlist uses `visitorIP(r, trustProxy)`. Postmark publishes source IPs (not verified here, operator supplies them). Cloudflare Workers have no stable egress IPs, so Worker hooks rely on the secret alone.
- Spam gate: if `max_spam_score` is set and `X-Spam-Score` exceeds it, return 200 and drop (a non-200 would trigger retries). Logged, not stored.
- The hook route does not use the session `Origin` check (no cookies involved) and uses its own body cap instead of the 1 MiB `limitBody`.
- No SSRF surface: the server only receives; it makes no outbound fetches.
- Rate limiting: existing per-IP bucket plus the ingest semaphore. A per-webhook limiter is deferred; a secret holder is already trusted with one room.
- Deleting a webhook deletes the webhook row, disables the bot user, and keeps messages (`author_id` is `ON DELETE RESTRICT`) and room membership so history still renders.

## CLI (same service layer)

```
chat-server webhook create --name N (--room ID | --user USERNAME [--self]) [--cidr C]... [--max-spam S]
chat-server webhook list
chat-server webhook enable|disable <id>
chat-server webhook rotate <id>     # new secret; old one dead immediately (no overlap window)
chat-server webhook delete <id>
```

`create` and `rotate` print the full path `/hooks/<id>/<secret>` once. `--room` adds the bot as `member` directly (operator action, audited). `--user` creates or reuses the bot-to-user DM; without `--self` it enforces `AllowPrivateMessages` / contact rules, with `--self` it bypasses them (ADR-020 notes this is an operator assertion, since the CLI has no chat identity). All actions write audit entries (`webhook.create|rotate|enable|disable|delete`).

## Config

`WEBHOOK_MAX_BODY_BYTES` (default 64 MiB, `sizeVar`). Ingest concurrency is a constant of 2, commented as a ceiling with an upgrade path.

## Code touch list

- `db/migrations/004_webhooks.sql`
- `internal/store/webhooks.go` (all queries through `Q`; new column list const with matching Scan)
- `internal/service/webhooks.go` (Create/Rotate/SetEnabled/Delete/List/Ingest + rendering helper)
- `internal/service/messages.go` (`SkipMentions`), `service.go` (`Storage.PutObject`)
- `internal/storage/storage.go` (`Disabled.PutObject`; `S3.PutObject` already existed)
- `internal/httpapi`: hook handler, hook chain, `logPath` in logging/recover
- `internal/cli/commands.go`: `webhook` command group
- `api/openapi.yaml`: `/hooks/{webhookId}/{secret}`, `InboundEmail`, role enum `bot` (`scripts/contract-check.py` must stay green)
- `docs/DECISIONS.md` (ADR-020..022), `docs/IMPLEMENTATION.md` tracker, `docs/CLI.md`, `docs/API.md`

## Test plan (authorization boundaries first)

- 403 with identical body for: unknown id, wrong secret, disabled, IP outside allowlist; no side effects.
- The secret never appears in captured log output (normal and panic paths).
- Bot cannot log in; no token or session can be minted for a bot.
- Dedupe: same `MessageID` twice gives one message; retry after simulated crash gives at most a duplicate, never a loss.
- Rendering: `<a@b.c>`, `&amp;`, triple backticks, control chars, 200 KB text, link in subject all produce a valid, accepted message.
- No mention rows and no mention push for `@user` in text.
- Attachments: stored and linked; over `MaxUploadBytes` skipped with a body line; 11 attachments; uploads disabled.
- 413 over cap; 503 with the semaphore full; 409 archived room; 404 bot removed.
- DM target: gate enforced without `--self`, bypassed with it.
- Migration applies cleanly; `go test ./scripts/` contract check passes.

## Deferred (not in v1)

Self-service webhook management over REST (this is where the DM privacy gate becomes a real check), per-address routing rules, HTML to text, per-webhook rate limit, delivery-row purge, rotation overlap window, streaming base64 decode, bot avatars, header-based secrets.
