# Models

The SDK abstracts model access behind two small interfaces, with two backends out of the box: the OpenAI **Responses API** (the SDK's native format) and the Anthropic **Messages API** (translated at the model boundary):

```go
// Model is one LLM: one call (or one streamed call) per turn.
type Model interface {
	Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error)
	StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error]
}

// ModelProvider resolves an agent's model name to a Model.
type ModelProvider interface {
	Model(modelName string) (Model, error)
}
```

## The OpenAI provider

```go
import "github.com/zzir/agents-go/models/openai"

provider := openai.NewProvider()                       // OPENAI_API_KEY from env
provider = openai.NewProvider(option.WithAPIKey("…"))  // any openai-go option
provider = provider.WithDefaultModel("gpt-4o-mini")    // model used when Agent.Model is empty
```

Unlike the Python SDK, this port ships **no built-in default model** ([differences](../explanation/migration_from_python.md)): a model must be named per agent (`Agent.Model`) or configured on the provider (`WithDefaultModel`). Resolving an agent that names no model, with no provider default set, returns a `*agents.UserError` — the caller is expected to be explicit about the model.

The OpenAI provider implements only the **Responses API** (`openai.ResponsesModel`); there is no Chat Completions fallback. Any OpenAI-compatible gateway that speaks the Responses API works via `option.WithBaseURL`, and you can drive several such providers in one run with retries and fallback — see [Retries, fallback, and multiple providers](#retries-fallback-and-multiple-providers).

## The Anthropic provider

```go
import "github.com/zzir/agents-go/models/anthropic"

provider := anthropic.NewProvider()                       // ANTHROPIC_API_KEY from env
provider = anthropic.NewProvider(option.WithAPIKey("…"))  // any anthropic-sdk-go option
provider = provider.WithDefaultModel("claude-opus-5")
```

`models/anthropic` is its own Go module (it carries the `anthropic-sdk-go` dependency, [decisions §5.7](../explanation/decisions.md#57-a-submodule-exists-only-to-keep-a-heavy-dependency-out-of-the-core)):

```bash
go get github.com/zzir/agents-go/models/anthropic
```

The adapter (`anthropic.MessagesModel`) translates the **Messages API** to and from the SDK's canonical Responses format at the model boundary, so tools, sessions, streaming, handoffs and structured output work unchanged. What to know:

- **Thinking.** `ModelSettings.Reasoning.Effort` maps to a thinking token budget (minimal 1024 / low 4096 / medium 16384 / high 32768 tokens). Thinking comes back as reasoning items; the signature rides in `encrypted_content` and survives session round-trips, so multi-turn extended thinking works.
- **max_tokens.** The Messages API requires it on every call; unset defaults to `anthropic.DefaultMaxTokens` (8192) — when a thinking budget would not fit under it, the default grows to budget + 8192. An explicit `MaxTokens` at or below the budget is a `*agents.UserError`, and models whose output cap is below 8192 (older Haiku generations) need an explicit `MaxTokens`. Thinking is also incompatible with `Temperature`/`TopP` and forced tool choice — those combinations are rejected up front.
- **Prompt caching.** On by default via the request-level `cache_control` marker — an agent loop resends a growing prefix every turn, which is exactly the shape caching pays for. `provider.WithPromptCaching(false)` opts out.
- **Unsupported settings fail loudly.** Responses-specific settings (`service_tier`, `verbosity`, `store`, `prompt_cache_*`, penalties, `truncation`, `top_logprobs`, `response_include`, `context_management`, `reasoning.summary`, `previous_response_id`, `conversation_id`, stored prompts) return a `*agents.UserError` instead of being silently dropped; `anthropic.Capabilities()` lists them. `ExtraBody` / `ExtraHeaders` remain the escape hatch for Anthropic-only parameters (`top_k`, `stop_sequences`, …). `Metadata` supports the one key the Messages API has: `user_id`.
- **Overflow.** A context overflow — a 400 "prompt is too long", or a response stopped with `model_context_window_exceeded` — surfaces as an error `agents.DetectContextOverflow` recognizes, so [compact-and-retry](sessions.md) works.
- **Retry classification.** `anthropic.RetryableError` / `anthropic.RetryAfter` mirror the OpenAI helpers for `agents.RetryPolicy`.
- **Refusals.** A `stop_reason: "refusal"` becomes ONE canonical refusal message and nothing else — the refusal text from the response (else `stop_details.explanation`, else a fixed line), with any partially generated `tool_use` blocks dropped so a refused response's actions never execute. `ModelRefusalError` and `model_refusal` error handlers fire exactly as on the OpenAI backend.

Cross-provider mixing goes through the standard decorators below: route prefixed model names (`anthropic/claude-opus-5`) with `NewRouterProvider`, or chain an Anthropic fallback behind an OpenAI primary. A runnable example is in `examples/anthropic`.

## Choosing models per agent

```go
fast := &agents.Agent{Name: "triage", Model: "gpt-4o-mini"}
deep := &agents.Agent{Name: "analyst", Model: "gpt-4o"}
```

Each agent's name is resolved through the run's provider. Two overrides bypass the provider:

- `Agent.ModelImpl` — an explicit `Model` instance for this agent (highest precedence; this is also how you plug in a fake model for tests).
- `RunOptions.Model` — one `Model` instance for every agent in the run.

## Retries, fallback, and multiple providers

A family of provider-agnostic decorators composes for resilience, multi-backend routing, and backend adaptation. None touch the run loop — they wrap a `Model` (or `ModelProvider`).

**Retry** — `agents.NewRetryModel(inner, policy)` retries transient failures with exponential backoff and jitter:

```go
policy := agents.RetryPolicy{
    MaxAttempts: 3,                     // total tries; 1 disables retry
    RetryIf:     openai.RetryableError, // retry 429/5xx/network, not 4xx or cancel
    RetryAfter:  openai.RetryAfter,     // honor a Retry-After header when present
}
model := agents.NewRetryModel(primary, policy)
```

Without `RetryIf`, the default (`agents.DefaultRetryIf`) retries every error except context cancellation; `openai.RetryableError` adds OpenAI-aware status-code classification. `openai.RetryAfter` understands both `Retry-After-Ms` (milliseconds, checked first — what OpenAI actually sends on short rate limits) and `Retry-After` (seconds or HTTP-date), and a server-suggested delay is always capped at the policy's `MaxDelay`.

> **One layer of retry.** The `openai-go` client can retry transient failures on its own, and stacked with `NewRetryModel` the two compose multiplicatively — a single transient error attempted up to `MaxAttempts × 3` times. Both `openai.NewProvider` and `anthropic.NewProvider` therefore **disable the client layer by default** (`WithMaxRetries(0)`): retry policy lives in `NewRetryModel`, where it is predictable and observable. A provider built without `NewRetryModel` performs no retries at all; to hand retries back to the transport instead, pass the option explicitly:
>
> ```go
> provider := openai.NewProvider(option.WithMaxRetries(2))
> ```

**Fallback** — `agents.NewFallbackModel(primary, backups...)` tries each backend in order until one succeeds, joining all errors if none do. Wrap each backend in a retry first so it exhausts its own retries before the chain advances:

```go
model := agents.NewFallbackModel(
    agents.NewRetryModel(primary, policy),
    agents.NewRetryModel(backup, policy),
)
agent := &agents.Agent{Name: "assistant", ModelImpl: model}
```

By default every error except context cancellation advances the chain. That is wasteful for deterministic failures (an invalid schema fails identically on every backend), so `WithShouldFallback` narrows the classification:

```go
model := agents.NewFallbackModel(primary, backup).
    WithShouldFallback(openai.RetryableError) // only transient errors advance
```

`NewFallbackProvider` accepts the same configuration and propagates it to every model it produces.

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

agents.Run(ctx, agent, input, agents.RunOptions{Model: agents.ModelOptions{Provider: router}})
// Agent.Model "groq/llama-3.3-70b" -> groqP.Model("llama-3.3-70b")
// Agent.Model "gpt-4o"             -> fallback openaiP.Model("gpt-4o")
```

> **Streaming caveat:** retry and fallback can only switch backends *before the first output event*. Events that carry no model output — lifecycle preamble (`response.created` / `response.in_progress` / `response.queued`) and terminal-failure events (`error` / `response.error` / `response.failed`) — don't commit an attempt: the decorators hold them back until output arrives, so a stream that dies early (a gateway `unexpected EOF`, a clean EOF with no terminal event — surfaced by the adapters as the same retryable truncation — or a `response.failed`) is replaced like a failed blocking call, and the consumer never sees the abandoned attempt's events. Once tokens start streaming a later error is passed through unchanged — already-sent output cannot be rolled back. Blocking `Respond` has no such limit, so it retries and falls back on any failure.

**Stream-only backends** — `agents.NewStreamOnlyModel(inner)` / `agents.NewStreamOnlyProvider(inner)` adapt a backend that rejects non-streaming requests (the ChatGPT Codex backend answers a non-streaming POST with 400): `Respond` runs the request as an internal stream and assembles the final `ModelResponse` from the terminal event; `StreamResponse` passes through. Compose it innermost, directly on the backend it adapts — decorators above it then see blocking-call failures as ordinary `Respond` errors:

```go
provider := agents.NewRetryProvider(
    agents.NewStreamOnlyProvider(codexBackend), // innermost, next to the backend
    policy,
)
```

A runnable example is in `examples/fallback`.

## Model settings

`ModelSettings` carries the provider knobs; `nil`/zero fields mean "leave unset" (use `new(expr)` for pointers):

```go
agent.ModelSettings = &agents.ModelSettings{
	Temperature:       new(0.3),
	TopP:              new(0.9),
	MaxTokens:         new(int64(2048)),
	ToolChoice:        agents.ToolChoiceAuto, // "auto" | "required" | "none" | a tool name
	ParallelToolCalls: new(true),
	Truncation:        agents.TruncationAuto,
	Reasoning:         &agents.Reasoning{Effort: "medium", Summary: "auto"},
	Verbosity:         "low",
	Store:             new(true),
	TopLogprobs:       new(int64(5)), // logprobs are included automatically
	Metadata:          map[string]string{"team": "support"},
	PromptCacheKey:    "chatbot-v3",         // forwarded as prompt_cache_key
	PromptCacheOptions: &agents.PromptCacheOptions{Mode: agents.PromptCacheModeExplicit, TTL: "30m"},
	ContextManagement: []agents.ContextManagement{{Type: "compaction"}},
	ExtraHeaders:      map[string]string{"X-Trace": "1"},
	ExtraBody:         map[string]any{"safety_identifier": "u_123"},
}
```

`RunOptions.Model.Settings` overlays per-run values over each agent's own (`Resolve` semantics).

Notes:

- `ToolChoice` of `"required"` or a specific tool name is automatically released after the agent calls a tool, preventing infinite loops — see [Agents](agents.md#stopping-after-tools-run). Any value other than `"auto"`/`"required"`/`"none"` is sent as a function tool name (the SDK has no provider-hosted tools).
- `PromptCacheKey` is forwarded as the Responses API `prompt_cache_key` to improve prompt-cache hit rates. Unlike the Python SDK, the runner **never auto-generates** one ([differences](../explanation/migration_from_python.md)): set it explicitly, or supply your own via `ExtraBody["prompt_cache_key"]`. Empty means unset.
- `PromptCacheOptions` configures prompt caching: `Mode` is `"implicit"` (default) or `"explicit"`, `TTL` is the minimum cache-entry lifetime (currently only `"30m"`). With `"explicit"` mode, mark cache breakpoints on input content parts (`prompt_cache_breakpoint`) to control which prompt prefixes are cached. nil leaves it unset.
- `ContextManagement` passes server-side context-management entries through to the Responses API — currently `ContextManagement{Type: "compaction", CompactThreshold: new(int64(...))}`, where a nil `CompactThreshold` leaves the threshold to the server. A nil/empty slice leaves it unset.
- The per-run overlay replaces `ExtraHeaders` / `ExtraQuery` / `ExtraBody` **wholesale** when the override sets them, rather than merging per key: a run-level `ExtraBody` shadows the agent's `ExtraBody` entirely, it does not union with it.

## Custom models

Implement `Model` to use any backend — return Responses-format output items and usage. The `models/modelkit` package holds the shared halves of that job: `modelkit.ParseInput` walks canonical input items into a neutral view, the item/event builders (`modelkit.MessageItem`, `modelkit.OutputItemDoneEvent`, `modelkit.CompletedEvent`, …) synthesize canonical output whose raw JSON round-trips, and `modelkit.Reject` enforces the fail-loud contract for unsupported settings. The golden test matrix in `modelkit/conformancetest` checks an adapter against the runner's consumption contract ([decisions §5.10](../explanation/decisions.md#510-non-responses-backends-adapt-at-the-model-boundary)) — both in-repo providers pass it. Event names come from the exported constants in `agents` (`agents.EventResponseCreated`, `agents.EventResponseOutputTextDelta`, `agents.EventResponseCompleted`, …), which spell the whole Responses stream vocabulary once — use them instead of string literals. A pass-through adapter that already holds a `responses.ResponseUsage` block can map it with `agents.UsageFromResponseUsage`, the same field table the runner and the conformance suite use.

```go
type myModel struct{}

func (myModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	// call your backend, translate to Responses output items
}
func (myModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	// yield Responses streaming events; end with a response.completed event
}
```

`ModelRequest` carries everything a turn needs: `SystemInstructions`, `Input`, `Settings`, `Tools`, `OutputSchema`, `Handoffs`, `PreviousResponseID`.

`ModelResponse` returns `Output` (the output items), `Usage`, `ResponseID` (chains calls via `previous_response_id`), and `RequestID` — the provider request identifier read from the transport response headers (OpenAI's `x-request-id`), handy for support and debugging. The OpenAI provider populates `RequestID` automatically; a custom `Model` leaves it empty when its backend supplies no such header.
