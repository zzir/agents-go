# Configuring the workbench

Where a configuration value of `agents-server` lives. There are three planes;
which one a value is on follows a rule, stated with its reasons in workbench
[invariant 54](../explanation/workbench-invariants.md):

| Plane | Set by | Changed by | For |
|---|---|---|---|
| **Process flags** | `agents-server --flag …` | restart | what must be fixed for the process's life |
| **Environment** | `AGENTS_* env var` | restart | keeping a secret off the command line |
| **Runtime settings** | Settings panel / API | live, no restart | what an operator tunes while running |

The SDK the workbench embeds reads no environment variable of its own — its
own contract, and the vendor libraries' exceptions to it, are
[spec §2.14](spec.md#214-the-sdk-reads-no-environment-variable)
([configuring the SDK](../howto/models.md#configuring-the-sdk)).

Authoritative defaults live at the source: `agents-server --help` for the
flags, and `GET /api/v1/setting-defs` (or
[`internal/settings/registry.go`](../../cmd/agents-server/internal/settings/registry.go))
for the runtime settings. The flag tables below are a convenience copy; the
settings table is generated from the registry (`make settings-doc` in
`cmd/agents-server`) and the settings tests fail while it is stale.

## Process flags

**Needed before the DB and API exist** (bind, storage, identity bootstrap):

| Flag | Default | Meaning |
|---|---|---|
| `--host` | `127.0.0.1` | Bind address (`0.0.0.0` for LAN) |
| `--port` | `9527` | HTTP port |
| `--db` | `data.db` | SQLite path, or a `postgres://` / `postgresql://` DSN |
| `--base-url` | — | Public origin of this server (required behind a proxy for OAuth) |
| `--auth` | `token` | `token` (one static token) or `oauth` (per-user login) |
| `--oauth-google-client-id` | — | Enables the Google login provider |
| `--oauth-google-client-secret` | — | Google secret (or env, below) |
| `--oauth-github-client-id` | — | Enables the GitHub login provider |
| `--oauth-github-client-secret` | — | GitHub secret (or env, below) |
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

Each is the fallback of one flag (flag wins), never a standalone knob
([invariant 54](../explanation/workbench-invariants.md)).

| Variable | Flag it backs | Holds |
|---|---|---|
| `AGENTS_TOKEN` | `--token` | The static auth token |
| `AGENTS_SECRET_KEY` | `--secret-key-file` | The 32-byte credential-sealing key (base64 or hex) |
| `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET` | `--oauth-google-client-secret` | The Google OAuth client secret |
| `AGENTS_OAUTH_GITHUB_CLIENT_SECRET` | `--oauth-github-client-secret` | The GitHub OAuth client secret |

Three variables the process does not define but honors, each a vendor
convention ([spec §2.14](spec.md#214-the-sdk-reads-no-environment-variable)):
`TZ` is the zone cron triggers tick in (Go's `time.Local`, reported by
`GET /api/v1/server`), `DOCKER_HOST` is where a docker sandbox with an empty
`host` dials, and `SSH_AUTH_SOCK` is the agent an `ssh://` sandbox with
`ssh_use_agent` authenticates through.

## Runtime settings

Tuned live through `PUT /api/v1/settings/:key` (admin-only, as is `DELETE` —
host configuration is [written by admins](protocol.md#authorization)) or the
Settings panel; a change takes effect on the next run, tick or connect. Every key is one entry in the
settings registry (invariant 40), which also decides masking and validation.
An empty value returns a key to its default. Keys by panel group:

<!-- settings-table:begin — generated from internal/settings/registry.go by `make settings-doc` -->
| Key | Group | Default | Meaning |
|---|---|---|---|
| `proxy_url` | network | — | All outbound API and MCP HTTP requests are routed through this proxy; a user:pass@ in it is masked on read. |
| `system_prompt` | prompt | — | Prepended to every agent, whether or not it binds a sandbox; an agent opts out under its own Instructions. Keep it tool-agnostic: file and shell tools mount only when a session binds a sandbox, so put machine- and tool-specific instructions in that sandbox's own Prompt, not here. |
| `trace_retention_days` | tracing | `30` | Trace events older than this many days are pruned daily, and a session left with none loses its stored payloads too. 0 keeps everything. |
| `trace_payload_retention_days` | tracing | — | A session whose newest trace span is older than this many days loses its stored model requests, replies and tool payloads daily. The spans stay with their timing, usage and errors, so the trace panel reads as before; only Replay has nothing to seed from. Unset or 0 keeps payloads as long as their spans. |
| `trace_include_sensitive_data` | tracing | `true` | Record prompts, outputs and tool arguments in new runs' traces; off keeps timing and usage only. |
| `trace_span_data_kb` | tracing | `1024` | How much of one stored payload element is kept — an input item, a reply item, a tool's arguments or result, the system prompt. Past it that element alone is replaced with a marker (the rest of the span stays) and a Replay of that call goes without it — raise it if you replay turns with very large items. Live updates to the browser are capped separately at 256KB per span; what they drop is still in the trace. Applies to new runs. |
| `log_sensitive_data` | logging | `false` | Include prompts, tool arguments and model output in SDK log records, shown at --log-level debug. |
| `approval_ttl_minutes` | limits | `1440` | How long a run may sit awaiting tool approval before it is expired and the wait is recorded in the transcript. 0 disables expiry. |
| `max_tasks_per_session` | limits | `6` | Concurrent live background tasks one session may have — a fat-finger guard against a runaway fan-out, not a scheduler. Read at each spawn, so a change applies to the next one. |
| `max_terminals_per_sandbox` | limits | `4` (max `32`) | Concurrent interactive terminals allowed on one sandbox — a fat-finger guard, not a scheduler. |
| `sandbox_idle_minutes` | limits | `30` | Stop a project's container after this many minutes with no run or terminal using it; 0 disables. The next run starts it again. |
| `s3_endpoint` | storage | — | S3-compatible API endpoint image attachments are uploaded to. The section saves as one form: Save probes the bucket end to end and refuses a broken configuration, Clear disables image input. |
| `s3_region` | storage | `auto` | Signing region. R2 and MinIO accept "auto"; AWS needs the bucket's real region. |
| `s3_bucket` | storage | — | Bucket the image objects live in. It must allow PUBLIC READS: model providers fetch attachment URLs anonymously, and so does anyone else holding a link. |
| `s3_access_key_id` | storage | — | S3 access key ID |
| `s3_secret_access_key` | storage | — | S3 secret access key (**secret**: masked on read) |
| `s3_public_base_url` | storage | — | Public prefix an object's key is appended to — what model providers and browsers fetch. Anyone with a URL can read that image. Changing this (or the bucket) without moving the objects breaks images already in session history. |
| `s3_path_style` | storage | `false` | On puts the bucket in the URL path (MinIO); off uses virtual-hosted addressing (AWS, R2). |
| `task_session_retention_days` | limits | — | A background task finished (completed, failed or cancelled) for longer than this many days loses its transcript and its row hourly; its result stays in the session it reported to. 0 keeps them forever. |
<!-- settings-table:end -->

`system_prompt`'s per-agent override is
[invariant 67](../explanation/workbench-invariants.md); the sweep behind
`task_session_retention_days` is
[invariant 72](../explanation/workbench-invariants.md); `max_tasks_per_session`
backs the SDK's `tasks.Config.MaxConcurrentPerParent` resolver.

The seven `storage` keys are **admin-only to read** (a member's
`GET /settings` leaves them out, `GET /settings/:key` is `403`) and are written
as **one group** through `PUT /api/v1/attachments/storage`, never key by key — see
[attachments](../howto/attachments.md#configuring-the-bucket) and
[invariant 58](../explanation/workbench-invariants.md).
