# Models

The SDK abstracts model access behind two small interfaces, with an OpenAI Responses API implementation out of the box:

```go
// Model is one LLM: one call (or one streamed call) per turn.
type Model interface {
	GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error)
	StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error]
}

// ModelProvider resolves an agent's model name to a Model.
type ModelProvider interface {
	GetModel(modelName string) (Model, error)
}
```

## The OpenAI provider

```go
import "github.com/zzir/agents-go/models/openai"

provider := openai.NewProvider()                       // OPENAI_API_KEY from env
provider = openai.NewProvider(option.WithAPIKey("…"))  // any openai-go option
provider = provider.WithDefaultModel("gpt-4o-mini")    // default when Agent.Model is empty (else "gpt-4o")
```

Only the **Responses API** is implemented (`openai.ResponsesModel`); there is no Chat Completions fallback ([differences](python_differences.md)). Any OpenAI-compatible gateway that speaks the Responses API works via `option.WithBaseURL`, and you can drive several such providers in one run with retries and fallback — see [Retries, fallback, and multiple providers](#retries-fallback-and-multiple-providers).

## Choosing models per agent

```go
fast := &agents.Agent{Name: "triage", Model: "gpt-4o-mini"}
deep := &agents.Agent{Name: "analyst", Model: "gpt-4o"}
```

Each agent's name is resolved through the run's provider. Two overrides bypass the provider:

- `Agent.ModelImpl` — an explicit `Model` instance for this agent (highest precedence; this is also how you plug in a fake model for tests).
- `RunOptions.Model` — one `Model` instance for every agent in the run.

## Retries, fallback, and multiple providers

Three provider-agnostic decorators compose for resilience and multi-backend routing. None touch the run loop — they wrap a `Model` (or `ModelProvider`).

**Retry** — `agents.NewRetryModel(inner, policy)` retries transient failures with exponential backoff and jitter:

```go
policy := agents.RetryPolicy{
    MaxAttempts: 3,                     // total tries; 1 disables retry
    RetryIf:     openai.RetryableError, // retry 429/5xx/network, not 4xx or cancel
    RetryAfter:  openai.RetryAfter,     // honor a Retry-After header when present
}
model := agents.NewRetryModel(primary, policy)
```

Without `RetryIf`, the default (`agents.DefaultRetryIf`) retries every error except context cancellation; `openai.RetryableError` adds OpenAI-aware status-code classification.

**Fallback** — `agents.NewFallbackModel(primary, backups...)` tries each backend in order until one succeeds, joining all errors if none do. Wrap each backend in a retry first so it exhausts its own retries before the chain advances:

```go
model := agents.NewFallbackModel(
    agents.NewRetryModel(primary, policy),
    agents.NewRetryModel(backup, policy),
)
agent := &agents.Agent{Name: "assistant", ModelImpl: model}
```

**Different vendors are just different providers** — same Responses protocol, different `base_url`/key:

```go
openaiP := openai.NewProvider() // OPENAI_API_KEY
groqP := openai.NewProvider(
    option.WithBaseURL("https://api.groq.com/openai/v1"),
    option.WithAPIKey(os.Getenv("GROQ_API_KEY")))
```

**Routing by name** — `agents.NewRouterProvider` sends each agent to a backend by a model-name prefix, so one run can mix vendors per agent:

```go
router := agents.NewRouterProvider(map[string]agents.ModelProvider{
    "openai": openaiP,
    "groq":   groqP,
}).WithFallback(openaiP)

agents.Run(ctx, agent, input, agents.RunOptions{ModelProvider: router})
// Agent.Model "groq/llama-3.3-70b" -> groqP.GetModel("llama-3.3-70b")
// Agent.Model "gpt-4o"             -> fallback openaiP.GetModel("gpt-4o")
```

> **Streaming caveat:** retry and fallback can only switch backends *before the first event is emitted*. Once tokens start streaming a later error is passed through unchanged — already-sent output cannot be rolled back. Blocking `GetResponse` has no such limit, so it retries and falls back on any failure.

A runnable example is in `examples/fallback`.

## Model settings

`ModelSettings` mirrors Python's dataclass; `nil`/zero fields mean "leave unset" (use `agents.Ptr` for pointers):

```go
agent.ModelSettings = &agents.ModelSettings{
	Temperature:       agents.Ptr(0.3),
	TopP:              agents.Ptr(0.9),
	MaxTokens:         agents.Ptr(int64(2048)),
	ToolChoice:        agents.ToolChoiceAuto, // "auto" | "required" | "none" | a tool name
	ParallelToolCalls: agents.Ptr(true),
	Truncation:        agents.TruncationAuto,
	Reasoning:         &agents.Reasoning{Effort: "medium", Summary: "auto"},
	Verbosity:         "low",
	Store:             agents.Ptr(true),
	TopLogprobs:       agents.Ptr(int64(5)), // logprobs are included automatically
	Metadata:          map[string]string{"team": "support"},
	ExtraHeaders:      map[string]string{"X-Trace": "1"},
	ExtraBody:         map[string]any{"safety_identifier": "u_123"},
}
```

`RunOptions.ModelSettings` overlays per-run values over each agent's own (`Resolve` semantics, matching Python).

Notes:

- `ToolChoice` of `"required"` or a specific tool name is automatically released after the agent calls a tool, preventing infinite loops — see [Agents](agents.md#tool-use-behavior). Any value other than `"auto"`/`"required"`/`"none"` is sent as a function tool name (the SDK has no provider-hosted tools).

## Custom models

Implement `Model` to use any backend — return Responses-format output items and usage:

```go
type myModel struct{}

func (myModel) GetResponse(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	// call your backend, translate to Responses output items
}
func (myModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.TResponseStreamEvent, error] {
	// yield Responses streaming events; end with a response.completed event
}
```

`ModelRequest` carries everything a turn needs: `SystemInstructions`, `Input`, `Settings`, `Tools`, `OutputSchema`, `Handoffs`, `PreviousResponseID`.
