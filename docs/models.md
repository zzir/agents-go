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

Only the **Responses API** is implemented (`openai.ResponsesModel`); there is no Chat Completions fallback and no LiteLLM-style multi-provider layer ([differences](python_differences.md)). Any OpenAI-compatible gateway that speaks the Responses API works via `option.WithBaseURL`.

## Choosing models per agent

```go
fast := &agents.Agent{Name: "triage", Model: "gpt-4o-mini"}
deep := &agents.Agent{Name: "analyst", Model: "gpt-4o"}
```

Each agent's name is resolved through the run's provider. Two overrides bypass the provider:

- `Agent.ModelImpl` — an explicit `Model` instance for this agent (highest precedence; this is also how you plug in a fake model for tests).
- `RunOptions.Model` — one `Model` instance for every agent in the run.

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

- `ToolChoice` of `"required"` or a specific tool name is automatically released after the agent calls a tool, preventing infinite loops — see [Agents](agents.md#tool-use-behavior).
- Hosted tool choices (`"file_search"`, `"web_search"`, …) are rejected with an error since hosted tools are unsupported.

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
