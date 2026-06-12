# Examples

Runnable examples live in [`examples/`](../examples). Each is a standalone `main` package:

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
```

| Example | Shows |
|---|---|
| [`examples/hello`](../examples/hello/main.go) | The minimal agent: provider, instructions, one `Run` |
| [`examples/tools`](../examples/tools/main.go) | Typed function tools via `NewFunctionTool` |
| [`examples/handoffs`](../examples/handoffs/main.go) | A triage agent delegating to specialists with `HandoffTo` |
| [`examples/streaming`](../examples/streaming/main.go) | Consuming `RunStreamed` events with `range` |
| [`examples/hitl`](../examples/hitl/main.go) | Human-in-the-loop: interrupt, approve/reject, resume |
| [`examples/sandbox`](../examples/sandbox/main.go) | An agent that writes and runs code in a local sandbox |
| [`sandbox/docker/example`](../sandbox/docker/example/main.go) | The Docker sandbox backend (separate module) |
| [`sandbox/k8s/example`](../sandbox/k8s/example/main.go) | The Kubernetes Jobs sandbox backend (separate module) |

The test suites are also worth reading as usage references — `agents/run_test.go` shows how to script a fake model (`Agent.ModelImpl`) for offline tests of your own agents.
