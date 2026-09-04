<div align="center">

# agents-go workbench

**Agent went wrong? See what the model saw, replay it, fork it.**

Local, one binary, no account. Your own model keys. MIT.

[![Release](https://img.shields.io/github/v/release/zzir/agents-go)](https://github.com/zzir/agents-go/releases)
[![CI](https://github.com/zzir/agents-go/actions/workflows/ci.yml/badge.svg)](https://github.com/zzir/agents-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzir/agents-go.svg)](https://pkg.go.dev/github.com/zzir/agents-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Get started](#get-started) ·
[What you get](#what-you-get) ·
[Docs](docs/) ·
[Examples](docs/tutorial/examples.md)

</div>

![A conversation with the Inspector open beside it: the trace of one run, a generation expanded](cmd/agents-server/screenshot.png)

## Get started

Download `agents-server` from [Releases](https://github.com/zzir/agents-go/releases), then:

```bash
./agents-server
```

Open `http://127.0.0.1:9527`, paste the token it printed, and add a provider key and an agent in Settings. Nothing here
needs Docker.

1. **Say something.** The reply streams in. **Context** shows what the model was sent; **Traces** shows every call with
   tokens and latency.
2. **Replay it.** Expand a generation in Traces, change the prompt or model, diff the result.
3. **Or fork it.** Branch the conversation at any turn. The original stays, either way.

[Running the workbench](docs/tutorial/workbench.md) has the full path, from building from source to sandboxes,
PostgreSQL and teams.

## What you get

- **Transcript**: an append-only session tree: a cancelled run keeps what finished, a paused one survives a restart.
- **Context**: what the model was sent, and what each part cost.
- **Traces**: model, tool, handoff and guardrail spans with tokens, latency and errors. No backend to run.
- **Replay**: re-run a generation with another prompt, model, settings or tools, diffed against the original. No session is touched.
- **Fork**: regenerate or branch at any turn: the parent kept, the other branch a click away.
- **Approvals**: every tool call visible. Approve a command once, trust that command, or trust the session.
- **Sandboxes**: a Docker container here or on a remote daemon, or any E2B-compatible service. Optional.

Also: MCP servers with OAuth, Agent Skills, background tasks, workflows, image
input, and a team mode with Google sign-in.

## Embed the runtime

The workbench is a Go program on top of the `agents` package; anything it does, your own program can do. Nothing above
needs this.

```bash
go get github.com/zzir/agents-go
```

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

The [Quickstart](docs/tutorial/quickstart.md) continues from here, and every capability has a runnable program
under [examples/](examples/). The API is pre-1.0: pin a version, and read the
[release notes](https://github.com/zzir/agents-go/releases) before moving.

## Docs

- [Running the workbench](docs/tutorial/workbench.md) — from a binary to a first conversation, then sandboxes and teams
- [Quickstart](docs/tutorial/quickstart.md) — the SDK, in Go
- [Design spec](docs/reference/spec.md) and [decisions](docs/explanation/decisions.md) — what is always true, and why
- [The map](docs/) — everything, sorted by what you came for

## License

[MIT](LICENSE)
