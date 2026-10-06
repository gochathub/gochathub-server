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
