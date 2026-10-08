# CLI Design

The CLI should be useful for server administration and automation.

Preferred command structure:

```text
chat-server
  serve

  migrate
  version

  user
    list
    create
    passwd
    enable
    disable
    delete

  room
    list
    create
    archive
    members
    invite

  token
    list
    create
    revoke

  webhook          (docs/WEBHOOKS.md)
    create --name N (--room ID | --user USERNAME [--self]) [--cidr C]... [--max-spam S]
    list
    enable|disable <id>
    rotate <id>
    delete <id>

  rcmigrate
    --archive <rocketchat-backup.archive>
    --dry-run
    --skip-files
```

Use interactive prompts only where useful.

Support noninteractive operation for automation:

```text
--json
--yes
--password-stdin
```

Never print passwords or token secrets to logs.

CLI must call the same service-layer methods used by HTTP handlers.

## rcmigrate (Rocket.Chat import)

One-shot/delta importer for a `mongodump --archive` of the `rocketchat`
database (`internal/migrate`, REQUIREMENTS in the gochathub-rocketchat-migration repo):

```text
DATABASE_URL=... S3_* ... chat-server rcmigrate --archive <backup.archive> [--dry-run] [--skip-files]
```

- First run requires an empty database or an existing `migrate.rc` watermark in `app_config`.
- Re-runs are idempotent: deterministic UUIDv5 ids from source ids; messages at/below the watermark are skipped. A `--skip-files` run leaves the message watermark unmoved (`files_pending`) so the next full run imports those messages with attachments.
- Passwords import as legacy `bcrypt$` hashes (bcrypt over SHA-256 hex — Rocket.Chat/Meteor scheme); `internal/pwd.Verify` accepts them and login transparently re-hashes to argon2id.
- 2FA enrollment (TOTP secret/backup hashes, email-2FA flag) is preserved in `migrate_rc_users`; migration `003_totp.sql` folds TOTP secrets and backup hashes into `user_totp`/`user_backup_codes` (email-2FA is dropped). Tokens and password material are not carried over.
- Lost authenticator: `chat-server user 2fa-reset <username>` removes the factor and revokes the user's sessions.
