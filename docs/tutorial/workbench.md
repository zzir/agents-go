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

The ones you reach for first:

| Flag                    | Default     | Description                                            |
|-------------------------|-------------|--------------------------------------------------------|
| `--host`                | `127.0.0.1` | Bind address (use `0.0.0.0` for LAN access)            |
| `--port`                | `9527`      | HTTP listen port                                       |
| `--db`                  | `data.db`   | SQLite file path, or a `postgres://` / `postgresql://` DSN |
| `--token`               | auto        | Auth token; randomly generated when omitted (or env `AGENTS_TOKEN`) |
| `--auth`                | `token`     | `token` (single static token) or `oauth` (per-user login — see [OAuth mode](../howto/workbench-auth.md#oauth-mode)) |
| `--log-level`           | `info`      | `debug`, `info`, `warn` or `error`                     |

`agents-server --help` lists every flag. For the full picture — all flags, the
`AGENTS_*` environment variables, and the runtime settings tuned live in the UI,
each with the rule for which plane it lives on — see
[Configuration reference](../reference/configuration.md).
