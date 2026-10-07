# Request Logs Plugin

Go dynamic-library plugin that records one structured row per upstream attempt and serves an OmniRoute-style request log table in the browser.

It declares three capabilities:

- `usage_plugin`: `usage.handle` fills provider, credential, token counts, latency, and TTFT.
- `request_lifecycle_plugin`: `request.complete` fills the terminal outcome and status, and captures rejected/canceled requests that never reach an executor.
- `management_api`: serves the HTML page and a JSON data endpoint.

Because `usage.handle` and `request.complete` carry different request IDs for the same attempt, rows are merged by `trace_id`.

## Endpoints

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/request-logs/logs` | none (page only) | Log table UI |
| `GET /v0/management/request-logs/entries` | management key | JSON `{entries, stats, providers, models}` |
| `POST /v0/management/request-logs/clear` | management key | Clear the in-memory buffer |

The page asks for the management key once and stores it in `localStorage`; data calls send it as `Authorization: Bearer <key>` or `X-Management-Key`.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    request-logs:
      enabled: true
      max_entries: 500                          # in-memory ring buffer size, default 500
      persist_file: plugins/state/request-logs.jsonl  # optional: JSONL durability
```

Records are held in a memory ring buffer. With `persist_file` set, every entry
snapshot is appended as one JSONL line and reloaded on startup (the last line
per request wins; rows still "running" at load are marked `stale`). `Clear
history` also truncates the file.

## Build

From the repository root on macOS:

```bash
mkdir -p plugins/darwin/$(go env GOARCH)
cd examples/plugin/request-logs/go
go build -buildmode=c-shared \
  -o ../../../../plugins/darwin/$(go env GOARCH)/request-logs.dylib .
rm -f ../../../../plugins/darwin/$(go env GOARCH)/request-logs.h
```

Linux (`.so`) requires building on Linux or in a Linux container because `c-shared` needs CGO:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26 \
  go build -buildmode=c-shared -buildvcs=false \
  -o plugins/linux/amd64/request-logs.so \
  ./examples/plugin/request-logs/go
```

The output filename is the plugin ID, so the artifact must be named `request-logs` for the configuration above.
