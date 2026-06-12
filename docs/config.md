# Configuring the SDK

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
res, err := agents.Run(ctx, agent, input, agents.RunOptions{ModelProvider: provider})
```

## Default model

The provider defaults to `gpt-4o` when an agent does not set `Agent.Model`. Override it once for all agents:

```go
provider := openai.NewProvider().WithDefaultModel("gpt-4o-mini")
```

Or pin a model per agent:

```go
agent := &agents.Agent{Name: "fast", Model: "gpt-4o-mini"}
```

## Model settings

`ModelSettings` carries optional parameters (temperature, tool_choice, max tokens, reasoning effort, …). All fields use pointers or zero-value-means-unset semantics so the provider default applies unless you set them; use `agents.Ptr` for pointer fields.

```go
agent.ModelSettings = &agents.ModelSettings{
	Temperature: agents.Ptr(0.2),
	MaxTokens:   agents.Ptr(int64(1024)),
}
```

A run-level override merges over each agent's own settings:

```go
res, err := agents.Run(ctx, agent, input, agents.RunOptions{
	ModelProvider: provider,
	ModelSettings: &agents.ModelSettings{Temperature: agents.Ptr(0.0)},
})
```

See [Models](models.md) for the full field list.

## Tracing

Tracing is opt-in: build a `*tracing.Tracer` and pass it in `RunOptions.Tracer`. Without one, tracing code paths are no-ops. See [Tracing](tracing.md).

## Debugging and logging

The SDK itself does not log. Failures surface as Go errors with typed wrappers (`*agents.MaxTurnsError`, `*agents.ModelBehaviorError`, guardrail tripwire errors, …) that you can match with `errors.As`; partial run state is attached as `RunErrorDetails`. See [Results](results.md#errors).
