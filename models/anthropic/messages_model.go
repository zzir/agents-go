package anthropic

import (
	"context"
	"fmt"
	"iter"
	"net/http"

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
}

// NewMessagesModel returns a MessagesModel for the given model name, using the
// provided MessageService (typically client.Messages). Prompt caching is
// enabled, matching NewProvider.
func NewMessagesModel(model string, client ant.MessageService) *MessagesModel {
	return &MessagesModel{model: model, client: client, promptCaching: true}
}

var _ agents.Model = (*MessagesModel)(nil)

// buildParams assembles the Messages API request from a ModelRequest.
func (m *MessagesModel) buildParams(req agents.ModelRequest) (ant.MessageNewParams, error) {
	if err := modelkit.Reject("anthropic", req, unsupportedFeatures...); err != nil {
		return ant.MessageNewParams{}, err
	}
	messages, err := convertInput(req.Input)
	if err != nil {
		return ant.MessageNewParams{}, err
	}
	tools := convertTools(req.Tools, req.Handoffs)

	messages, leadingSystem := hoistLeadingSystem(messages)
	params := ant.MessageNewParams{
		Model:    ant.Model(m.model),
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
	if tc, ok := convertToolChoice(settingsToolChoice(req.Settings), settingsParallel(req.Settings), hasTools); ok {
		params.ToolChoice = tc
	}
	if req.OutputSchema != nil && !req.OutputSchema.IsPlainText() {
		params.OutputConfig.Format = ant.JSONOutputFormatParam{Schema: req.OutputSchema.JSONSchema()}
	}
	if m.promptCaching {
		params.CacheControl = ant.NewCacheControlEphemeralParam()
	}
	if err := applySettings(&params, req.Settings); err != nil {
		return ant.MessageNewParams{}, err
	}
	return params, nil
}

// applySettings overlays the model settings, including the max_tokens /
// thinking-budget coupling: max_tokens is mandatory on this API, and an
// enabled thinking budget must stay strictly below it.
func applySettings(params *ant.MessageNewParams, s *agents.ModelSettings) error {
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
			budget, ok := thinkingBudgets[s.Reasoning.Effort]
			if !ok {
				return agents.NewUserError("anthropic: unknown reasoning effort %q", s.Reasoning.Effort)
			}
			// The API's thinking incompatibilities, rejected here for the same
			// reason max_tokens/budget is: a preflightable 400 should be a
			// UserError naming the conflict, not a remote error naming a field.
			if s.Temperature != nil || s.TopP != nil {
				return agents.NewUserError(
					"anthropic: temperature/top_p cannot be combined with thinking (reasoning.effort) — unset the sampling overrides or the effort")
			}
			switch s.ToolChoice {
			case "", agents.ToolChoiceAuto, agents.ToolChoiceNone:
			default:
				return agents.NewUserError(
					"anthropic: tool_choice %q cannot be combined with thinking — the API allows only auto/none while thinking", s.ToolChoice)
			}
			if explicitMax && maxTokens <= budget {
				return agents.NewUserError(
					"anthropic: max_tokens (%d) must exceed the thinking budget for reasoning effort %q (%d) — raise max_tokens or lower the effort",
					maxTokens, s.Reasoning.Effort, budget)
			}
			if !explicitMax && maxTokens <= budget {
				// The default cap grows instead of failing: the user asked for
				// thinking, not for a max_tokens negotiation.
				maxTokens = budget + DefaultMaxTokens
			}
			params.Thinking = ant.ThinkingConfigParamUnion{
				OfEnabled: &ant.ThinkingConfigEnabledParam{BudgetTokens: budget},
			}
		}
	}
	params.MaxTokens = maxTokens
	return nil
}

// applyMetadata maps canonical metadata onto the Messages metadata object,
// which has exactly one field. Any other key would be silently discarded by
// the API even if sent, so it is rejected here instead.
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
	if s == nil {
		return nil
	}
	var opts []option.RequestOption
	for k, v := range s.ExtraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	for k, v := range s.ExtraQuery {
		opts = append(opts, option.WithQuery(k, v))
	}
	for k, v := range s.ExtraBody {
		// WithJSONSet interprets the key as an sjson path; escape its
		// metacharacters so k is a literal top-level key (same contract as the
		// OpenAI provider's ExtraBody).
		opts = append(opts, option.WithJSONSet(modelkit.EscapeJSONPath(k), v))
	}
	return opts
}

// Respond implements agents.Model.
func (m *MessagesModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	var httpResp *http.Response
	opts := append(requestOptions(req.Settings), option.WithResponseInto(&httpResp))
	msg, err := m.client.New(ctx, params, opts...)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages: %w", err)
	}
	status, incompleteReason, err := statusFromStopReason(msg.StopReason)
	if err != nil {
		return nil, err
	}
	items, err := convertOutput(msg)
	if err != nil {
		return nil, err
	}
	var requestID string
	if httpResp != nil {
		requestID = httpResp.Header.Get("Request-Id")
	}
	return &agents.ModelResponse{
		Output:           items,
		Usage:            usageFromMessage(msg.Usage),
		ResponseID:       msg.ID,
		RequestID:        requestID,
		Status:           status,
		IncompleteReason: incompleteReason,
	}, nil
}

// StreamResponse implements agents.Model. The Messages SSE stream is
// translated event by event into canonical response.* events; see stream.go.
func (m *MessagesModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		params, err := m.buildParams(req)
		if err != nil {
			yield(nil, err)
			return
		}
		stream := m.client.NewStreaming(ctx, params, requestOptions(req.Settings)...)
		defer stream.Close()
		synthesizeStream(stream, yield)
	}
}
