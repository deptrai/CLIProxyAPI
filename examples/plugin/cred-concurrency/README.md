# cred-concurrency (Go)

Per-credential concurrency limiter. Caps parallel in-flight requests per auth
record; requests beyond the cap fall back to the next candidate/provider by
priority order.

## How it works

- `scheduler.pick`: candidates arrive in host priority order. The plugin picks
  the first candidate whose in-flight count is below its limit. Credentials
  without a limit have unlimited capacity, so they act as the natural fallback.
  If every limited candidate is busy and no unlimited candidate remains, the
  pick is rejected with `auth_unavailable` so the conductor moves to the next
  provider group.
- `usage.handle`: every completed `UsageRecord` releases one slot for its
  `AuthID` (fires on success and failure).
- Leak guard: a slot older than `slot_ttl` without a usage record is reclaimed
  (covers picks that never reached upstream execution).

## Limits

Read from each auth JSON's `max-concurrent` (or `max_concurrent`) top-level
key, or overridden via plugin config:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cred-concurrency:
      enabled: true
      slot_ttl: "30m"          # optional, default 30m
      limits:                  # optional per-auth overrides
        devin-account-a.json: 1
        devin-account-b.json: 3
```

Credentials with no limit configured are never capped. If no candidate carries
a limit, the plugin delegates to the builtin scheduler (`Handled: false`).

## Build

```bash
cd go && go build -buildmode=c-shared -o cred-concurrency.dylib .  # macOS
GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o cred-concurrency.so .  # Linux
```

Drop the artifact into the server's `plugins/` dir and enable it under
`plugins.configs` as shown above.
