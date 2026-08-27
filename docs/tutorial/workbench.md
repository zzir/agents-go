# Running the workbench

`agents-server` is the Go-native agent workbench: one binary, your data, run by
you. This page gets it running; [deployment](../howto/workbench-deploy.md) and
[authentication](../howto/workbench-auth.md) take it further, and the
[wire protocol](../reference/protocol.md) documents its API surface.

Grab a prebuilt binary for your platform from the
[Releases](https://github.com/zzir/agents-go/releases) page, or build from
source. The web UI is compiled into the binary via `go:embed`, and the built
`internal/web/frontend/dist` is not checked in — so a source build must build
the frontend first; `make build` does both (npm required):

```bash
cd cmd/agents-server
make build          # npm install + build the SPA, then go build with it embedded
./agents-server --port 9527
```

On startup the server prints an auto-generated auth token. Open
`http://127.0.0.1:9527` to access the web UI.

### Flags

| Flag                    | Default     | Description                                            |
|-------------------------|-------------|--------------------------------------------------------|
| `--host`                | `127.0.0.1` | Bind address (use `0.0.0.0` for LAN access)            |
| `--port`                | `9527`      | HTTP listen port                                       |
| `--db`                  | `data.db`   | SQLite file path, or a `postgres://` / `postgresql://` DSN |
| `--token`               | auto        | Auth token; randomly generated when omitted            |
| `--max-tasks`           | `0`         | Max live background tasks per session (`0` = default 6) |
| `--log-level`           | `info`      | `debug`, `info`, `warn` or `error`                     |
| `--log-format`          | `text`      | `text` for a terminal, `json` for a collector          |
| `--base-url`            | —           | Public origin, `scheme://host[:port]` — see [Deployment](../howto/workbench-deploy.md#deployment) |
| `--trusted-proxies`     | —           | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For` is believed for client IPs |
| `--auth`                | `token`     | `token` (single static token) or `oauth` (per-user login — see [OAuth mode](../howto/workbench-auth.md#oauth-mode)) |
| `--oauth-google-client-id` / `--oauth-google-client-secret` | — | Google login credentials (secret also via `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET`) |
| `--allowed-domains` / `--allowed-emails` | — | OAuth admission allowlist (comma-separated)  |
| `--bootstrap-admin`     | —           | Email that signs in as admin; implicitly admitted        |
| `--audit-retention-days` | `0`        | Prune audit log entries older than N days (0 = keep forever) — see [Audit log](../howto/workbench-auth.md#audit-log) |
| `--secret-key-file`     | —           | File holding the 32-byte key that seals stored credentials; or env `AGENTS_SECRET_KEY` — see [Secret handling](../reference/protocol.md#secret-handling) |
