# Configuring the workbench

Where a configuration value of `agents-server` lives. There are three planes;
which one a value is on follows a rule, stated with its reasons in workbench
[invariant 54](../explanation/workbench-invariants.md):

| Plane | Set by | Changed by | For |
|---|---|---|---|
| **Process flags** | `agents-server --flag …` | restart | what must be fixed for the process's life |
| **Environment** | `AGENTS_* env var` | restart | keeping a secret off the command line |
| **Runtime settings** | Settings panel / API | live, no restart | what an operator tunes while running |

The SDK the workbench embeds reads **no** environment variable of its own —
everything is passed in ([spec §2.14](spec.md), [SDK config](../howto/config.md)).
The only model-backend key read from the environment is openai-go's
`OPENAI_API_KEY`, and that is the provider's, not the server's.

Authoritative defaults live at the source: `agents-server --help` for the
flags, and `GET /api/v1/setting-defs` (or
[`internal/settings/registry.go`](../../cmd/agents-server/internal/settings/registry.go))
for the runtime settings. The tables below are a convenience copy.

## Process flags

**Needed before the DB and API exist** (bind, storage, identity bootstrap):

| Flag | Default | Meaning |
|---|---|---|
| `--host` | `127.0.0.1` | Bind address (`0.0.0.0` for LAN) |
| `--port` | `9527` | HTTP port |
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

**Security-load-bearing** — never settable through the running API:

| Flag | Default | Meaning |
|---|---|---|
| `--token` | auto-generated | The static auth token (or env, below) |
| `--trusted-proxies` | none | Proxy IPs/CIDRs whose `X-Forwarded-For` is believed |
| `--audit-retention-days` | `0` (keep forever) | Prune audit entries older than N days |

## Environment variables

Each is the fallback of one flag (flag wins, then env); there is no
standalone env knob and no "every flag is also an env var".

| Variable | Flag it backs | Holds |
|---|---|---|
| `AGENTS_TOKEN` | `--token` | The static auth token |
| `AGENTS_SECRET_KEY` | `--secret-key-file` | The 32-byte credential-sealing key (base64 or hex) |
| `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET` | `--oauth-google-client-secret` | The Google OAuth client secret |

## Runtime settings

Tuned live through `PUT /api/v1/settings/:key` or the Settings panel; a change
takes effect on the next run, tick or connect. Every key is one entry in the
settings registry (invariant 40), which also decides masking and validation.
An empty value returns a key to its default. Keys by panel group:

| Key | Group | Default | Meaning |
|---|---|---|---|
| `proxy_url` | network | — | Route all outbound API/MCP HTTP through this proxy |
| `system_prompt` | prompt | — | Instructions prepended to every agent |
| `trace_retention_days` | tracing | `30` | Prune trace events older than N days (`0` keeps everything; also checked at startup) |
| `trace_include_sensitive_data` | tracing | `true` | Record prompts/outputs/tool args in stored traces |
| `trace_span_data_kb` | tracing | `8192` | How much of a span's request/response is stored; past it the payload is replaced with a marker |
| `log_sensitive_data` | logging | `false` | Include conversation content in the SDK's own log records |
| `approval_ttl_minutes` | limits | `1440` | How long a run waits for tool approval before expiring (`0` disables) |
| `max_tasks_per_session` | limits | `6` | Concurrent live background tasks per session; read at each spawn (backs the SDK's `tasks.Config.MaxConcurrentPerParent` resolver) |
| `max_terminals_per_sandbox` | limits | `4` (max `32`) | Concurrent interactive terminals on one sandbox |
| `sandbox_idle_minutes` | limits | `30` | Stop a project's container after N idle minutes (`0` disables) |
| `s3_endpoint` | storage | — | S3-compatible API endpoint image attachments are uploaded to (absolute http(s) URL) |
| `s3_region` | storage | `auto` | Signing region (`auto` for R2 and MinIO; AWS needs the bucket's region) |
| `s3_bucket` | storage | — | Bucket the image objects live in; must allow public reads |
| `s3_access_key_id` | storage | — | Access key id |
| `s3_secret_access_key` | storage | — | Secret access key (**secret**: masked on read) |
| `s3_public_base_url` | storage | — | Public prefix an object's key is appended to (absolute http(s) URL) |
| `s3_path_style` | storage | `false` | Path-style addressing (MinIO) instead of virtual-hosted (AWS, R2) |

The seven `storage` keys are written as **one group** through
`PUT /api/v1/attachments/storage`, never key by key — see
[attachments](../howto/attachments.md#configuring-the-bucket) and
[invariant 58](../explanation/workbench-invariants.md).
