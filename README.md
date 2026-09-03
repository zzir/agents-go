<div align="center">

# agents-go workbench

**Go agents. Local first.**

The Go-native agent workbench you run yourself. One binary, your data: see
exactly what the model saw, then replay or fork any turn. Sandboxes behind
approvals, workflows, MCP. Solo or as a team.

[![Release](https://img.shields.io/github/v/release/zzir/agents-go)](https://github.com/zzir/agents-go/releases)
[![CI](https://github.com/zzir/agents-go/actions/workflows/ci.yml/badge.svg)](https://github.com/zzir/agents-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzir/agents-go.svg)](https://pkg.go.dev/github.com/zzir/agents-go)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Get started](#get-started) ·
[What you get](#what-you-get) ·
[Docs](docs/) ·
[Embed the SDK](#embed-it-the-agents-go-sdk)

</div>

![agents-go workbench — a conversation with the trace panel open beside it](cmd/agents-server/screenshot.png)

## Get started

The first conversation is the binary, one API key and a browser. Nothing
here needs Docker. (The repository is `agents-go`; the program you run is
`agents-server`.)

1. **Get the binary.** Download the archive for your OS and CPU from
   [Releases](https://github.com/zzir/agents-go/releases) and extract it; the
   `agents-server` binary is at the top level. To build from source instead,
   see [Running the workbench](docs/tutorial/workbench.md#get-a-binary).

2. **Run it.**

   ```bash
   ./agents-server
   ```

   It listens on `http://127.0.0.1:9527` and prints an auth token at startup.
   Open the address in a browser and paste the token into the login screen.
   State is one SQLite file, `data.db`, in the directory you ran it from.

3. **Add a provider and an agent.** Settings → Providers → New: an OpenAI or
   Anthropic API key, a ChatGPT sign-in, or any Responses-compatible endpoint
   by base URL. Settings → Agents → New: a name, that provider, a model and
   instructions; leave the rest at its defaults.

4. **Say something.** New chat, type, and the reply streams in. Then open the
   Inspector from the top bar: **Traces** is every model call, tool call and
   handoff with tokens and latency; **Context** is what the context window
   holds and what each part costs.

> **macOS.** Gatekeeper refuses the unsigned binary;
> `xattr -d com.apple.quarantine ./agents-server` clears it.

Sandboxes, PostgreSQL, the container image, a reverse proxy and teams all
come after that first conversation:
[Running the workbench](docs/tutorial/workbench.md) continues from here, and
[Deploying](docs/howto/workbench-deploy.md) takes it off localhost.

## What you get

Built for one person or a small team running their own agents. One process,
one database — SQLite by default, your own PostgreSQL if you prefer — and the
server itself shells out to nothing.

- **The transcript is the truth.** A session is an append-only tree: every turn
  is persisted as it completes, so a cancelled run keeps what finished and a
  paused one survives a restart. Regenerate or fork any turn; the abandoned
  branch stays switchable.
- **A context lens and traces, no backend.** What actually goes into the
  context window and what each part costs, plus agent, tool, sandbox and
  guardrail spans with tokens and latency, in a panel beside the conversation.
  Nothing to collect, nothing to run.
- **Replay any generation.** Re-run a traced model call with a different
  prompt, model, settings or tools, streamed, diffed against the original.
  No session is touched.

Together these close the debug loop: see what the model saw, change it,
re-run — one call, one turn, or a fork — and diff the results. Around it:

- **Real sandboxes behind an approval gate.** A Docker container on this
  machine or a remote daemon, or a sandbox on any E2B-compatible service. The
  model reads files, edits them with `apply_patch`, runs commands. Approve a
  command once, trust that command, or trust the session; open a terminal into
  it from the browser, or export the whole working tree as a tar.
- **Work that outlives the turn.** `spawn_task` sub-agents that wake the parent
  when they finish and resume in place when they fail; workflows as fixed step
  sequences, started by the model, by hand, by cron or by a signed webhook.
- **MCP, skills and the rest.** MCP servers over streamable HTTP with OAuth,
  Agent Skills, projects, memories, guardrails.
- **Image input.** Paste or drop screenshots into the chat; bytes go to your
  S3-compatible bucket, the model gets a URL, history keeps a reference
  ([details](docs/howto/attachments.md)).
- **Solo, or a team.** `--auth oauth` swaps the single token for Google sign-in
  and an allowlist: sessions and configuration are per person, an admin
  publishes what everyone shares, and credentials are sealed at rest
  ([details](docs/howto/workbench-auth.md)).

## Embed it: the agents-go SDK

The workbench is a Go program on top of the `agents` package. Anything it does,
your own program can do:

```bash
go get github.com/zzir/agents-go
```

> **Pre-1.0 API notice.** Until v1.0.0 a minor release may rename or remove
> exported identifiers — pin the version. Breaking renames are batched, and the
> [release notes](https://github.com/zzir/agents-go/releases) carry every old
> spelling beside the new.

```go
package main

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("You are a helpful assistant."),
		Model:        "gpt-4o",
	}

	res, err := agents.RunSync(context.Background(), agent, "Hello!", agents.RunOptions{
		// Provider reads OPENAI_API_KEY.
		Model: agents.ModelOptions{Provider: openai.NewProvider()},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.FinalOutputString())
}
```

The [Quickstart](docs/tutorial/quickstart.md) continues from here — handoffs,
guardrails, typed tools, structured output, streaming, approvals. A few things
worth knowing it does:

- An argument struct becomes a tool's JSON schema; an agent becomes a tool
  ([Tools](docs/howto/tools.md))
- A paused run serializes to JSON and resumes in another process
  ([Human-in-the-loop](docs/howto/human_in_the_loop.md))
- A run is a range-able iterator you can steer mid-flight
  ([Streaming](docs/howto/streaming.md))
- Session history persists to memory, SQLite/Postgres, or the model
  provider's own store ([Sessions](docs/howto/sessions.md))
- A scripted `Model` tests your agents with no API key
  ([Testing](docs/howto/testing.md))

The [full map](docs/) covers the rest: MCP, sandboxes, skills, tracing,
background tasks, model retry/fallback/routing, and run middleware.

The core is one small module; the heavier capabilities are opt-in submodules
([packages](docs/explanation/architecture.md#packages)). Arriving from the
OpenAI Agents SDK? Start at
[Coming from Python?](docs/explanation/migration_from_python.md).

The SDK stands alone — it never depends on or reports to the workbench
([scope §1.2](docs/explanation/scope.md#12-non-goals)).

Nearly every SDK capability has a runnable example under
[examples/](examples/):

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello      # minimal agent
go run ./examples/handoffs   # triage agent → specialists
go run ./examples/hitl       # pause, approve, resume

go test ./examples/testing   # scripted model — no API key needed
```

## License

[MIT](LICENSE)
