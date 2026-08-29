# Configuring the SDK

Everything the SDK reads is passed in. There is no global registry, no init
hook and no ambient default — a run is configured by the `RunOptions` you hand
it and the agent it is given, which is what makes two differently-configured
runs safe to execute concurrently in one process.

This page covers the knobs that are not about a single capability: API keys and
clients, logging, and the environment. For a capability's own options see its
page.

The SDK reads **no environment variable of its own** (spec §2.14): the `agents`
package calls no `os.Getenv`, so nothing ambient changes a run's behavior behind
your back. The only environment default in play is openai-go's own
`OPENAI_API_KEY`, resolved inside the OpenAI provider below — a vendor library's
contract you opt into by not passing a key, not something this SDK reads.

## API keys and clients

The SDK never reads global state behind your back: model access is configured per run via a `ModelProvider` (or per agent via `ModelImpl`).

The OpenAI provider reads `OPENAI_API_KEY` from the environment by default, and accepts any [openai-go](https://github.com/openai/openai-go) request option:

```go
import (
	"github.com/openai/openai-go/v3/option"
	"github.com/zzir/agents-go/models/openai"
)

// Default: key from OPENAI_API_KEY.
provider := openai.NewProvider()

// Explicit key, custom base URL (e.g. a gateway or compatible endpoint):
provider = openai.NewProvider(
	option.WithAPIKey("sk-..."),
	option.WithBaseURL("https://my-gateway.example.com/v1"),
)
```

Every agent in a run resolves its model through the provider passed in `RunOptions`:

```go
res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{Model: agents.ModelOptions{Provider: provider}})
```

## Default model

There is no built-in default model: an agent that sets no `Agent.Model` fails with a `UserError` unless the provider was given a default. Configure one for all agents with:

```go
provider := openai.NewProvider().WithDefaultModel("gpt-4o-mini")
```

Or pin a model per agent:

```go
agent := &agents.Agent{Name: "fast", Model: "gpt-4o-mini"}
```

## Model settings

`ModelSettings` carries optional parameters (temperature, tool_choice, max tokens, reasoning effort, …). All fields use pointers or zero-value-means-unset semantics so the provider default applies unless you set them; use `new(expr)` for pointer fields.

```go
agent.ModelSettings = &agents.ModelSettings{
	Temperature: new(0.2),
	MaxTokens:   new(int64(1024)),
}
```

A run-level override merges over each agent's own settings:

```go
res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Model: agents.ModelOptions{Provider: provider, Settings: &agents.ModelSettings{Temperature: new(0.0)}},
})
```

See [Models](models.md) for the full field list.

## Tracing

Tracing is opt-in: build a `*tracing.Tracer` and pass it in `RunOptions.Observe.Tracer`. Without one, tracing code paths are no-ops. See [Tracing](tracing.md).

## Debugging and logging

The SDK is silent by default and never writes to `slog.Default()`; opt in to its structured logging with `RunOptions.Log` — see [Logging](logging.md). Failures surface as Go errors with typed wrappers (`*agents.MaxTurnsError`, `*agents.ModelBehaviorError`, guardrail tripwire errors, …) that you can match with `errors.As`; a failed run's partial state rides on `*agents.RunError`. See [Results](results.md#errors).
