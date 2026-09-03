# Models

## Configuring the SDK

Everything the SDK acts on is passed in — `RunOptions`, the `Agent`, its
`ModelSettings`, the provider's constructor options — and the `agents` package
reads no environment variable of its own ([spec §2.14](../reference/spec.md#214-the-sdk-reads-no-environment-variable)),
so two differently-configured runs are safe in one process. The knobs that are
not about a single capability:

- **Model access** — a `ModelProvider` in `RunOptions.Model.Provider`, or a `Model` per agent (`Agent.ModelImpl`); constructors [below](#the-openai-provider).
- **Default model** — none is built in: name one per agent (`Agent.Model`) or on the provider (`WithDefaultModel`) — [choosing models per agent](#choosing-models-per-agent).
- **Provider knobs** — `ModelSettings` on the agent, overlaid per run — [model settings](#model-settings).
- **Tracing** and **logging** — both opt-in and both silent by default: [Tracing](tracing.md), [Logging](logging.md).
- **Failures** — Go errors with typed wrappers matched by `errors.As`; a failed run's partial state rides on `*agents.RunError` — [Results](results.md#errors).

The SDK abstracts model access behind two small interfaces — `Model` (one call, or one streamed call, per turn) and `ModelProvider` (a model name to a `Model`); see [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#Model). Two backends ship: the OpenAI **Responses API** (the SDK's native format) and the Anthropic **Messages API** (translated at the model boundary).

## The OpenAI provider

```go
import "github.com/zzir/agents-go/models/openai"

provider := openai.NewProvider()                       // OPENAI_API_KEY from env (openai-go's own default)
provider = openai.NewProvider(option.WithAPIKey("…"))  // any openai-go option
provider = openai.NewProvider(option.WithBaseURL("https://my-gateway.example.com/v1")) // a gateway or compatible endpoint
provider = provider.WithDefaultModel("gpt-4o-mini")    // model used when Agent.Model is empty
```

Unlike the Python SDK, this port ships **no built-in default model** ([differences](../explanation/migration_from_python.md)): a model must be named per agent (`Agent.Model`) or configured on the provider (`WithDefaultModel`). Resolving an agent that names no model, with no provider default set, returns a `*agents.UserError` — the caller is expected to be explicit about the model.

The OpenAI provider implements only the **Responses API** (`openai.ResponsesModel`); there is no Chat Completions fallback. Any OpenAI-compatible gateway that speaks the Responses API works via `option.WithBaseURL`, and you can drive several such providers in one run with retries and fallback — see [Retries, fallback, and multiple providers](#retries-fallback-and-multiple-providers).

A response the API reports as `failed`, or `incomplete` for any reason other than the output-token limit, is an error — but one that still billed tokens. The error wraps the terminal `*agents.ModelBehaviorError` in a `*modelkit.UsageError` carrying that usage (`errors.As` reaches either), so a caller accounting for spend can add what the run never received.

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

The adapter (`anthropic.MessagesModel`) translates the **Messages API** to and from the SDK's canonical Responses format at the model boundary, so tools, sessions, streaming, handoffs and structured output work unchanged.

### Anthropic backend defaults

- **Thinking.** `ModelSettings.Reasoning.Effort` maps to a thinking token budget (minimal 1024 / low 4096 / medium 16384 / high 32768 tokens). Thinking comes back as reasoning items; the signature rides in `encrypted_content` and survives session round-trips, so multi-turn extended thinking works.
- **max_tokens.** The Messages API requires it on every call; unset defaults to `anthropic.DefaultMaxTokens` (8192) — when a thinking budget would not fit under it, the default grows to budget + 8192. An explicit `MaxTokens` at or below the budget is a `*agents.UserError`, and models whose output cap is below 8192 (older Haiku generations) need an explicit `MaxTokens`. Thinking is also incompatible with `Temperature`/`TopP` and forced tool choice — those combinations are rejected up front.
- **Prompt caching.** On by default via the request-level `cache_control` marker — an agent loop resends a growing prefix every turn, which is exactly the shape caching pays for. `provider.WithPromptCaching(false)` opts out.
- **System messages mid-history** travel as `mid_conv_system` blocks in system turns (the Messages API has no `system` input role; top-of-run instructions use the top-level `system` parameter).
- **Reasoning blobs** carry an adapter prefix in `encrypted_content` (`thinking_signature:` / `redacted_thinking:`); a blob without a recognized prefix is another provider's reasoning and is dropped on replay rather than sent as a bogus signature.
- **Budgets, not the native effort parameter**, because budgets work on every thinking-capable Claude model and the SDK keeps no model-capability tables (scope §1.2). `service_tier` is unsupported: Anthropic's values do not correspond to the Responses tiers, and a guessed mapping would buy a different QoS than configured.
- **A compaction summary at the very front** is hoisted into the top-level `system` parameter so the first message stays a user/assistant turn.
- **`stop_reason: max_tokens`** becomes `incomplete` / `max_output_tokens`; `stop_reason: refusal` becomes one canonical refusal message (decisions §5.49).

### What the translation does

- **Unsupported settings fail loudly.** Responses-specific settings (`service_tier`, `verbosity`, `store`, `prompt_cache_*`, `truncation`, `top_logprobs`, `response_include`, `context_management`, `reasoning.summary`, `previous_response_id`, `conversation_id`, stored prompts) return a `*agents.UserError` instead of being silently dropped; `anthropic.Capabilities()` lists them. `ExtraBody` / `ExtraHeaders` remain the escape hatch for Anthropic-only parameters (`top_k`, `stop_sequences`, …). `Metadata` supports the one key the Messages API has: `user_id`.
- **Overflow.** A context overflow — a 400 "prompt is too long", or a response stopped with `model_context_window_exceeded` — surfaces as an error `agents.DetectContextOverflow` recognizes, so [compact-and-retry](sessions.md) works.
- **Retry classification.** `anthropic.RetryableError` / `anthropic.RetryAfter` mirror the OpenAI helpers for `agents.RetryPolicy`.
- **Refusals and message shape.** A `stop_reason: "refusal"` becomes one canonical refusal message with any partial `tool_use` dropped, consecutive `text` blocks become one message item, and when streaming the per-item `output_item.done` events wait for the stop reason ([decisions §5.49](../explanation/decisions.md#549-the-anthropic-adapter-decides-its-output-items-at-the-stop-reason)).
- **Lossy input translations.** Replaying a canonical history to the Messages API drops what it cannot carry, silently rather than failing the run: a `refusal` part is omitted (a refusal is not an answer the model gave, so it is not replayed as assistant text), `input_image.detail` and output-text `annotations` are dropped, a `reasoning` item without a signature is skipped, and the citations a response carried are discarded on the way in. Everything else that has no equivalent (`input_file` by file id, an unknown item type) is a `*agents.UserError`.

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

Without `RetryIf`, the default (`agents.DefaultRetryIf`) retries every error except context cancellation and deadline expiry (`context.Canceled`, `context.DeadlineExceeded`); `openai.RetryableError` adds OpenAI-aware status-code classification. `openai.RetryAfter` understands both `Retry-After-Ms` (milliseconds, checked first — what OpenAI actually sends on short rate limits) and `Retry-After` (seconds or HTTP-date); a server-suggested delay longer than the policy's `MaxDelay` ends the retries with that attempt's error rather than being clamped to the cap.

> **One layer of retry.** Both `openai.NewProvider` and `anthropic.NewProvider` build their clients with `WithMaxRetries(0)`, so a provider without `NewRetryModel` performs no retries at all; pass `option.WithMaxRetries(n)` explicitly to hand retries back to the transport ([decisions §5.22](../explanation/decisions.md#522-retry-policy-lives-in-one-layer)).

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

> **Streaming caveat:** retry and fallback can only switch backends *before the first output event* — a stream that dies early is replaced like a failed blocking call, but once tokens have streamed a later error passes through unchanged ([decisions §5.16](../explanation/decisions.md#516-a-severed-stream-retries-only-before-output-with-the-preamble-held-back)).

**Stream-only backends** — `agents.NewStreamOnlyModel(inner)` / `agents.NewStreamOnlyProvider(inner)` adapt a backend that rejects non-streaming requests (the ChatGPT Codex backend answers a non-streaming POST with 400): `Respond` runs the request as an internal stream and assembles the final `ModelResponse` from the terminal event; `StreamResponse` passes through. Compose it innermost, directly on the backend it adapts — decorators above it then see blocking-call failures as ordinary `Respond` errors:

```go
provider := agents.NewRetryProvider(
    agents.NewStreamOnlyProvider(codexBackend), // innermost, next to the backend
    policy,
)
```

A runnable example is in `examples/fallback`.

## Model settings

`ModelSettings` carries the provider knobs; `nil`/zero fields mean "leave unset" (use `new(expr)` for pointers). The full field list is on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#ModelSettings).

```go
agent.ModelSettings = &agents.ModelSettings{
	Temperature: new(0.3),
	MaxTokens:   new(int64(2048)),
	Reasoning:   &agents.Reasoning{Effort: "medium", Summary: "auto"},
}
```

`RunOptions.Model.Settings` overlays per-run values over each agent's own (`Resolve` semantics):

```go
res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Model: agents.ModelOptions{Provider: provider, Settings: &agents.ModelSettings{Temperature: new(0.0)}},
})
```

Notes:

- `ToolChoice` of `"required"` or a specific tool name is automatically released after the agent calls a tool, preventing infinite loops — see [Agents](agents.md#stopping-after-tools-run). Any value other than `"auto"`/`"required"`/`"none"` is sent as a function tool name (the SDK has no provider-hosted tools).
- `PromptCacheKey` is forwarded as the Responses API `prompt_cache_key` to improve prompt-cache hit rates. Unlike the Python SDK, the runner **never auto-generates** one ([differences](../explanation/migration_from_python.md)): set it explicitly, or supply your own via `ExtraBody["prompt_cache_key"]`. Empty means unset.
- `PromptCacheOptions` configures prompt caching: `Mode` is `"implicit"` (default) or `"explicit"`, `TTL` is the minimum cache-entry lifetime (currently only `"30m"`). With `"explicit"` mode, mark cache breakpoints on input content parts (`prompt_cache_breakpoint`) to control which prompt prefixes are cached. nil leaves it unset.
- `ContextManagement` passes server-side context-management entries through to the Responses API — currently `ContextManagement{Type: "compaction", CompactThreshold: new(int64(...))}`, where a nil `CompactThreshold` leaves the threshold to the server. A nil/empty slice leaves it unset.
- The per-run overlay replaces `ExtraHeaders` / `ExtraQuery` / `ExtraBody` **wholesale** when the override sets them, rather than merging per key: a run-level `ExtraBody` shadows the agent's `ExtraBody` entirely, it does not union with it.

## Custom models

Implement `Model` to use any backend — return Responses-format output items and usage. The `models/modelkit` package holds the shared halves of that job: `modelkit.ParseInput` walks canonical input items into a neutral view, the item/event builders (`modelkit.MessageItem`, `modelkit.OutputItemDoneEvent`, `modelkit.CompletedEvent`, …) synthesize canonical output whose raw JSON round-trips, and `modelkit.Reject` enforces the fail-loud contract for unsupported settings. The golden test matrix in `modelkit/conformancetest` checks an adapter against the runner's consumption contract ([spec §2.15](../reference/spec.md#215-the-model-adapter-contract)) — both in-repo providers pass it. Event names come from the exported constants in `agents` (`agents.EventResponseCreated`, `agents.EventResponseOutputTextDelta`, `agents.EventResponseCompleted`, …), which spell the whole Responses stream vocabulary once — use them instead of string literals. A pass-through adapter that already holds a `responses.ResponseUsage` block can map it with `agents.UsageFromResponseUsage`, the same field table the runner and the conformance suite use.

```go
type myModel struct{}

func (myModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	// call your backend, translate to Responses output items
}
func (myModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	// yield Responses streaming events; end with a response.completed event
}
```

`ModelRequest` and `ModelResponse` are documented on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#ModelRequest). Two things to know: `PreviousResponseID`, `ConversationID` and `Prompt` are Responses-only (the Anthropic adapter rejects them), and `ModelResponse.RequestID` is the provider request id read from the transport headers (OpenAI `x-request-id`, Anthropic `request-id`) — a custom `Model` leaves it empty when its backend supplies none.
