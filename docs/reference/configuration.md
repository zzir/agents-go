# Configuring the workbench

This is the map of **where a configuration value lives** in `agents-server`.
There are three planes, and which one a value is on is a rule, not a habit
(workbench [invariant 54](../explanation/workbench-invariants.md)):

| Plane | Set by | Changed by | For |
|---|---|---|---|
| **Process flags** | `agents-server --flag …` | restart | what must be fixed for the process's life |
| **Environment** | `AGENTS_* env var` | restart | keeping a secret off the command line |
| **Runtime settings** | Settings panel / API | live, no restart | what an operator tunes while running |

The SDK the workbench embeds has its own, separate rule: it reads **no**
environment variable of its own — everything is passed in
([spec §2.14](spec.md), [SDK config](../howto/config.md)). The only
model-backend key from the environment is openai-go's `OPENAI_API_KEY`, and
that is the provider's, not the server's.

Authoritative defaults live at the source, not here: run `agents-server --help`
for the flags, and open the Settings panel (or read
[`internal/settings/registry.go`](../../cmd/agents-server/internal/settings/registry.go))
for the runtime settings. This page is the map and the *reasons*; the values
below are a convenience copy.

## Process flags

A value is a flag when it must be fixed for the process's life — for one of two
reasons. (A cap the SDK consumes is *not* one of them: see the runtime settings
below, and [invariant 54](../explanation/workbench-invariants.md).)

**Needed before the DB and API exist** (bind, storage, identity bootstrap):

| Flag | Default | Meaning |
|---|---|---|
| `--host` | `127.0.0.1` | Bind address (`0.0.0.0` for LAN) |
| `--port` | `9527` | HTTP port |
| `--preview-port` | `0` (= `port+1`) | Isolated sandbox-preview origin; must differ from `--port` |
| `--preview-base-url` | — | Public origin of the preview listener behind a proxy |
| `--db` | `data.db` | SQLite path, or a `postgres://` DSN |
| `--base-url` | — | Public origin of this server (required behind a proxy for OAuth) |
| `--auth` | `token` | `token` (one static token) or `oauth` (per-user login) |
| `--oauth-google-client-id` | — | Enables the Google login provider |
| `--oauth-google-client-secret` | — | Google secret (or env, below) |
| `--allowed-domains` | — | Comma-separated email domains admitted to OAuth |
| `--allowed-emails` | — | Comma-separated emails admitted to OAuth |
| `--bootstrap-admin` | — | Email that signs in as admin (the recovery hatch) |
| `--secret-key-file` | — | File holding the 32-byte key that seals stored credentials (or env, below) |
| `--log-level` | `info` | `debug` / `info` / `warn` / `error` |
| `--log-format` | `text` | `text` / `json` |

**Security-load-bearing** — mutating it through the running API would defeat its
purpose:

| Flag | Default | Meaning |
|---|---|---|
| `--token` | auto-generated | The static auth token (or env, below). A secret, never API-settable |
| `--trusted-proxies` | none | Proxy IPs/CIDRs whose `X-Forwarded-For` is believed |
| `--audit-retention-days` | `0` (keep forever) | Prune audit entries older than N days — the log of config changes must not be shortened through the API it records |

## Environment variables

The server reads env vars for **one** purpose: keeping a secret off `argv`/`ps`.
Each is the fallback of an explicit flag (flag wins, then env), never a
standalone knob. There is no viper-style "every flag is also an env var".

| Variable | Flag it backs | Holds |
|---|---|---|
| `AGENTS_TOKEN` | `--token` | The static auth token |
| `AGENTS_SECRET_KEY` | `--secret-key-file` | The 32-byte credential-sealing key (base64 or hex) |
| `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET` | `--oauth-google-client-secret` | The Google OAuth client secret |

## Runtime settings

Everything an operator tunes live, without a restart — enforced by a server loop
that re-reads it, so a change takes effect on the next run, tick or connect.
Every key is one entry in the settings registry (invariant 40); the panel,
masking and validation all derive from it. Current keys:

| Key | Default | Meaning |
|---|---|---|
| `proxy_url` | — | Route all outbound API/MCP HTTP through this proxy |
| `system_prompt` | — | Instructions prepended to all agents |
| `trace_retention_days` | `30` | Prune trace events older than N days (`0` keeps everything) |
| `trace_include_sensitive_data` | `true` | Record prompts/outputs/tool args in stored traces |
| `trace_span_data_kb` | `8192` | How much of a span's request/response is stored |
| `log_sensitive_data` | `false` | Include conversation content in the SDK's own log records |
| `approval_ttl_minutes` | `1440` | How long a run waits for tool approval before expiring (`0` disables) |
| `max_tasks_per_session` | `6` | Concurrent live background tasks per session; read at each spawn (feeds the SDK's `tasks.Config.MaxConcurrentPerParent` resolver) |
| `max_terminals_per_sandbox` | `4` | Concurrent interactive terminals on one sandbox |
| `sandbox_idle_minutes` | `30` | Stop a project's container after N idle minutes (`0` disables) |
| `preview_enabled` | `false` | Let a project owner reach a port inside its sandbox through this server |
