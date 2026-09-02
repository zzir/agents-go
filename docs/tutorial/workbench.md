# Running the workbench

`agents-server` is the Go-native agent workbench: one binary, your data, run by
you. This page takes it from nothing to a first conversation with a sandbox
and a trace. [Deployment](../howto/workbench-deploy.md) and
[authentication](../howto/workbench-auth.md) take it further; the
[wire surface](../reference/protocol.md) documents its API; every flag,
environment variable and runtime setting is in the
[configuration reference](../reference/configuration.md).

## Get a binary

Grab a prebuilt binary for your platform from the
[Releases](https://github.com/zzir/agents-go/releases) page, or build from
source. The web UI is compiled into the binary via `go:embed`, and the built
`internal/web/frontend/dist` is not checked in — so a source build must build
the frontend first; `make build` does both (npm required):

```bash
cd cmd/agents-server
make build          # npm install + build the SPA, then go build with it embedded
./agents-server
```

It listens on `http://127.0.0.1:9527` (`--host 0.0.0.0` for the LAN, `--port`
to move) and keeps its state in `data.db` in the directory you ran it from
(`--db`; a `postgres://` DSN uses PostgreSQL instead). On startup it prints an
auto-generated auth token — open the address in a browser and paste the token
into the login screen. `agents-server --help` lists every flag.

## Add a provider and an agent

Everything you configure lives in one place: the sidebar's **Settings** entry
(also in the account menu at the sidebar's foot) opens the Settings hub, a
dialog with a panel per thing.

1. **Providers** → New: an OpenAI or Anthropic API key, a ChatGPT sign-in, or
   any Responses-compatible endpoint by base URL. Save.
2. **Agents** → New: a name, the provider you just made, a model, and
   instructions. Leave the rest at its defaults. Save.

That is enough to talk. **New** (the sidebar's + button) opens a conversation;
pick the agent in the composer, type, and the reply streams in. The
conversation appears in the sidebar, and its `…` menu pins, renames, forks or
deletes it. The top
bar's three icons open the Inspector beside the conversation: **Traces**
(every model call, tool call and handoff with tokens and latency — expand a
generation span to see exactly what the model was sent, and **Replay** it with
a different prompt or model), **Context** (what the context window holds and
what each part costs) and **Tasks** (background work).

## Give it a sandbox

An agent that should read files and run commands needs a working tree, and a
working tree lives on a sandbox:

1. **Settings → Sandboxes** → New: type `docker` with this machine's daemon
   (leave the host empty) or a remote one over SSH, an image, and — if you
   like — a **prompt** describing the machine. Or type `e2b` for any service
   speaking the E2B API. **Test** runs `echo ok` in a throw-away container.
2. In a conversation, the composer's **Project** picker creates a project on
   that sandbox — one user's working tree, mounted at `/workspace`. The first
   run binds the conversation to it for good.

Now the agent has `read_file`, `write_file`, `list_files`, `apply_patch` and
`exec_command`. Put `["exec_command"]` in the agent's **Approve tools** and
every command pauses for you: approve this call, trust this exact command for
the session, or trust every command. The top
bar's project menu opens a **terminal** into the same container, and exports
the working tree as a tar.

## The rest of the hub

Each panel in Settings is a thing you can add: **MCP** servers (streamable
HTTP, with OAuth), **Skills** (`SKILL.md` documents, imported from a GitHub
repository or written here), **Memory**, **Guardrails**, and **General** — the
runtime settings (a proxy, a system prompt, trace retention, the caps, the
attachment bucket that turns on [image input](../howto/attachments.md)). Your
**Account** panel holds your profile and personal access tokens.

Running as a team (`--auth oauth`, [authentication](../howto/workbench-auth.md))
adds two things an admin sees in the same hub: an **All members** toggle on
the shared panels — Providers, Agents, MCP, Skills, Workflows — that lists
every member's rows for publishing and transfer, and an **Administration**
group with Members, Sessions, Projects and Audit logs.

## Automate it

Sidebar → **Workflows** opens the hub for work that outlives a turn: fixed
step sequences you define once and start with `/workflow <name> <brief>` in a
conversation, run on a schedule or from a signed webhook with a trigger, and
watch under Runs. [Workflows](../howto/workflows.md) walks it end to end.
