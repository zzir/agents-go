<div align="center">

# agents-go workbench

The Go-native agent workbench you run yourself: see what the model saw, replay it, fork it.

**Go agents. Local first.**

[![Release](https://img.shields.io/github/v/release/zzir/agents-go)](https://github.com/zzir/agents-go/releases)
[![CI](https://github.com/zzir/agents-go/actions/workflows/ci.yml/badge.svg)](https://github.com/zzir/agents-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzir/agents-go.svg)](https://pkg.go.dev/github.com/zzir/agents-go)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Get started](#get-started) ·
[What you get](#what-you-get) ·
[Docs](docs/)

</div>

![agents-go workbench — a conversation with the trace panel open beside it](cmd/agents-server/screenshot.png)

## Get started

The first conversation is the binary, one API key and a browser. Nothing
here needs Docker. (The repository is `agents-go`; the program you run is
`agents-server`.)

1. **Get the binary** from [Releases](https://github.com/zzir/agents-go/releases) and extract it.
2. **Run it.** `./agents-server` listens on `http://127.0.0.1:9527` and prints an auth token; open the address in a browser and paste the token.
3. **Add a provider and an agent** in Settings: an OpenAI or Anthropic key, a ChatGPT sign-in or any Responses-compatible endpoint; then a name, that provider, a model and instructions.
4. **Say something.** New chat, type, and the reply streams in; the Inspector in the top bar shows every model call with tokens and latency, and what the context window holds.

[Running the workbench](docs/tutorial/workbench.md) has each step in full —
building from source, the macOS Gatekeeper note, the flags — and continues to
sandboxes, PostgreSQL and teams; [Deploying](docs/howto/workbench-deploy.md)
takes it off localhost.

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
agent := &agents.Agent{
	Name:         "assistant",
	Instructions: agents.StaticInstructions("You are a helpful assistant."),
	Model:        "gpt-4o",
}

res, err := agents.RunSync(ctx, agent, "Hello!", agents.RunOptions{
	Model: agents.ModelOptions{Provider: openai.NewProvider()}, // reads OPENAI_API_KEY
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(res.FinalOutputString())
```

The complete program is [examples/hello](examples/hello/main.go). The
[Quickstart](docs/tutorial/quickstart.md) continues from here — handoffs,
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

Nearly every SDK capability has a runnable example under [examples/](examples/)
— `go run ./examples/hello` to start; the full list, and which ones need more
than `OPENAI_API_KEY`, is in [Examples](docs/tutorial/examples.md).

## License

[MIT](LICENSE)
