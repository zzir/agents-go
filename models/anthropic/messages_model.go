package anthropic

import (
	"context"
	"iter"
	"strings"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// MessagesModel calls a model through the Anthropic Messages API. It
// implements agents.Model.
type MessagesModel struct {
	model         string
	client        ant.MessageService
	promptCaching bool
	// budgetThinking sends reasoning effort as a thinking token budget instead
	// of adaptive thinking — see Provider.WithBudgetThinking.
	budgetThinking bool
	// thinkingBinding asks the API to drop a replayed thinking block bound to
	// an edited prefix — see Provider.WithThinkingBinding.
	thinkingBinding bool
}

// NewMessagesModel returns a MessagesModel for the given model name, using the
// provided MessageService (typically client.Messages). Prompt caching and
// thinking binding are enabled, matching NewProvider.
func NewMessagesModel(model string, client ant.MessageService) *MessagesModel {
	return &MessagesModel{model: model, client: client, promptCaching: true, thinkingBinding: true}
}

var _ agents.Model = (*MessagesModel)(nil)

// buildParams assembles the Messages API request from a ModelRequest; the
// fingerprint is the prefix this request's thinking blocks are bound to.
func (m *MessagesModel) buildParams(req agents.ModelRequest) (ant.MessageNewParams, string, error) {
	if err := modelkit.Reject("anthropic", req, unsupportedFeatures...); err != nil {
		return ant.MessageNewParams{}, "", err
	}
	tools := convertTools(req.Tools, req.Handoffs)
	fingerprint := prefixFingerprint(req.SystemInstructions, tools)
	messages, err := convertInput(req.Input, fingerprint)
	if err != nil {
		return ant.MessageNewParams{}, "", err
	}

	messages, leadingSystem := hoistLeadingSystem(messages)
	params := ant.MessageNewParams{
		Model:    m.model,
		Messages: messages,
	}
	if req.SystemInstructions != "" {
		params.System = []ant.TextBlockParam{{Text: req.SystemInstructions}}
	}
	params.System = append(params.System, leadingSystem...)
	if len(tools) > 0 {
		params.Tools = tools
	}
	// "has tools" means tools ON THE WIRE: handoffs are sent as tools too, so
	// a handoff-only agent's parallel-calls setting must still be carried.
	hasTools := len(req.Tools) > 0 || len(req.Handoffs) > 0
	settings := modelkit.Settings(req.Settings)
	if tc, ok := convertToolChoice(settings.ToolChoice, settings.ParallelToolCalls, hasTools); ok {
		params.ToolChoice = tc
	}
	if req.OutputSchema != nil && !req.OutputSchema.IsPlainText() {
		params.OutputConfig.Format = ant.JSONOutputFormatParam{Schema: req.OutputSchema.JSONSchema()}
	}
	if m.promptCaching {
		params.CacheControl = ant.NewCacheControlEphemeralParam()
	}
	if err := applySettings(&params, req.Settings, m.budgetThinking); err != nil {
		return ant.MessageNewParams{}, "", err
	}
	return params, fingerprint, nil
}

// applySettings overlays the model settings. max_tokens is mandatory here, and
// thinking spends from it: the default grows with the effort, an explicit one
// stands as given.
func applySettings(params *ant.MessageNewParams, s *agents.ModelSettings, budgetThinking bool) error {
	maxTokens := DefaultMaxTokens
	explicitMax := false
	if s != nil && s.MaxTokens != nil {
		maxTokens = *s.MaxTokens
		explicitMax = true
	}

	if s != nil {
		if s.Temperature != nil {
			params.Temperature = ant.Float(*s.Temperature)
		}
		if s.TopP != nil {
			params.TopP = ant.Float(*s.TopP)
		}
		if err := applyMetadata(params, s.Metadata); err != nil {
			return err
		}
		if s.Reasoning != nil && s.Reasoning.Effort != "" {
			var room int64
			var err error
			if budgetThinking {
				room, err = applyBudgetThinking(params, s, maxTokens, explicitMax)
			} else {
				room, err = applyAdaptiveThinking(params, s.Reasoning.Effort)
			}
			if err != nil {
				return err
			}
			// The default cap grows instead of failing: the user asked for
			// thinking, not for a max_tokens negotiation.
			if !explicitMax && maxTokens <= room {
				maxTokens = room + DefaultMaxTokens
			}
		}
	}
	params.MaxTokens = maxTokens
	return nil
}

// applyAdaptiveThinking maps an effort onto adaptive thinking plus
// output_config.effort and returns the room the default max_tokens leaves it.
func applyAdaptiveThinking(params *ant.MessageNewParams, effort agents.ReasoningEffort) (int64, error) {
	wire, ok := adaptiveEfforts[effort]
	if !ok {
		if effort == agents.ReasoningEffortNone {
			return 0, agents.NewUserError(
				"anthropic: reasoning effort \"none\" cannot be expressed — some models think whatever the request says; leave the effort unset instead")
		}
		return 0, agents.NewUserError("anthropic: unknown reasoning effort %q", effort)
	}
	params.Thinking = ant.ThinkingConfigParamUnion{
		OfAdaptive: &ant.ThinkingConfigAdaptiveParam{Display: ant.ThinkingConfigAdaptiveDisplaySummarized},
	}
	params.OutputConfig.Effort = wire
	return thinkingRoom[wire], nil
}

// applyBudgetThinking maps an effort onto a thinking token budget, the form
// models before adaptive thinking take, and returns that budget.
func applyBudgetThinking(params *ant.MessageNewParams, s *agents.ModelSettings, maxTokens int64, explicitMax bool) (int64, error) {
	effort := s.Reasoning.Effort
	budget, ok := thinkingBudgets[effort]
	if !ok {
		switch effort {
		case agents.ReasoningEffortNone, agents.ReasoningEffortXhigh, agents.ReasoningEffortMax:
			return 0, agents.NewUserError(
				"anthropic: reasoning effort %q has no thinking budget — use minimal, low, medium or high, or turn budget thinking off", effort)
		}
		return 0, agents.NewUserError("anthropic: unknown reasoning effort %q", effort)
	}
	// Manual thinking's incompatibilities, preflighted as UserErrors naming the
	// conflict.
	if s.Temperature != nil || s.TopP != nil {
		return 0, agents.NewUserError(
			"anthropic: temperature/top_p cannot be combined with a thinking budget (reasoning.effort) — unset the sampling overrides or the effort")
	}
	switch s.ToolChoice {
	case "", agents.ToolChoiceAuto, agents.ToolChoiceNone:
	default:
		return 0, agents.NewUserError(
			"anthropic: tool_choice %q cannot be combined with a thinking budget — the API allows only auto/none while thinking", s.ToolChoice)
	}
	if explicitMax && maxTokens <= budget {
		return 0, agents.NewUserError(
			"anthropic: max_tokens (%d) must exceed the thinking budget for reasoning effort %q (%d) — raise max_tokens or lower the effort",
			maxTokens, effort, budget)
	}
	params.Thinking = ant.ThinkingConfigParamUnion{
		OfEnabled: &ant.ThinkingConfigEnabledParam{BudgetTokens: budget},
	}
	return budget, nil
}

// applyMetadata maps canonical metadata onto the Messages metadata object,
// which has one field; any other key is rejected rather than silently dropped.
func applyMetadata(params *ant.MessageNewParams, metadata map[string]string) error {
	for k, v := range metadata {
		if k != "user_id" {
			return agents.NewUserError(
				"anthropic: metadata key %q is not supported by the Messages API (only \"user_id\" is)", k)
		}
		params.Metadata = ant.MetadataParam{UserID: ant.String(v)}
	}
	return nil
}

// requestOptions builds per-request options from the model settings' extra
// headers, query parameters and body fields.
func requestOptions(s *agents.ModelSettings) []option.RequestOption {
	return modelkit.ExtraOptions(s, option.WithHeader, option.WithQuery, option.WithJSONSet)
}

// Respond implements agents.Model, served from the stream: the SDK refuses a
// non-streaming request with a large max_tokens — see decisions §5.15.
func (m *MessagesModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	return agents.NewStreamOnlyModel(m).Respond(ctx, req)
}

// StreamResponse implements agents.Model. The Messages SSE stream is
// translated event by event into canonical response.* events; see stream.go.
func (m *MessagesModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		params, fingerprint, err := m.buildParams(req)
		if err != nil {
			yield(nil, err)
			return
		}
		opts := requestOptions(req.Settings)
		if m.thinkingBinding && (params.Thinking.OfAdaptive != nil || params.Thinking.OfEnabled != nil) {
			opts = append(opts, bindingOptions(req.Settings)...)
		}
		stream := m.client.NewStreaming(ctx, params, opts...)
		defer stream.Close()
		synthesizeStream(ctx, stream, yield, fingerprint)
	}
}

// thinkingBindingBeta is the beta the block_binding field needs: without it
// the API refuses the field.
const thinkingBindingBeta = "thinking-binding-controls-2026-08-01"

// bindingOptions asks the API to drop a thinking block whose prefix no longer
// matches. They go AFTER the caller's options: an ExtraHeaders anthropic-beta
// is set, not added, so the beta joins that value instead of losing to it.
func bindingOptions(s *agents.ModelSettings) []option.RequestOption {
	opts := []option.RequestOption{
		option.WithJSONSet("thinking.block_binding.prefix_mismatch_behavior", "drop_block"),
	}
	if s != nil {
		for k, v := range s.ExtraHeaders {
			if !strings.EqualFold(k, "anthropic-beta") {
				continue
			}
			if !strings.Contains(v, thinkingBindingBeta) {
				v += "," + thinkingBindingBeta
			}
			return append(opts, option.WithHeader("anthropic-beta", v))
		}
	}
	return append(opts, option.WithHeaderAdd("anthropic-beta", thinkingBindingBeta))
}
