package openai

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zzir/agents-go/agents"
)

// ResponsesModel calls a model through the OpenAI Responses API. It implements
// agents.Model.
type ResponsesModel struct {
	model  string
	client responses.ResponseService
}

// NewResponsesModel returns a ResponsesModel for the given model name, using the
// provided ResponseService (typically client.Responses).
func NewResponsesModel(model string, client responses.ResponseService) *ResponsesModel {
	return &ResponsesModel{model: model, client: client}
}

var _ agents.Model = (*ResponsesModel)(nil)

// buildParams assembles the Responses API request from a ModelRequest.
func (m *ResponsesModel) buildParams(req agents.ModelRequest) (responses.ResponseNewParams, error) {
	tools, err := convertTools(req.Tools, req.Handoffs)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(m.model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: req.Input},
	}
	if req.SystemInstructions != "" {
		params.Instructions = oai.String(req.SystemInstructions)
	}
	if req.PreviousResponseID != "" {
		params.PreviousResponseID = oai.String(req.PreviousResponseID)
	}
	if req.ConversationID != "" {
		params.Conversation = responses.ResponseNewParamsConversationUnion{
			OfString: oai.String(req.ConversationID),
		}
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	tc, ok, err := convertToolChoice(settingsToolChoice(req.Settings))
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	if ok {
		params.ToolChoice = tc
	}

	text, hasFormat := responseFormat(req.OutputSchema)
	if v := settingsVerbosity(req.Settings); v != "" {
		text.Verbosity = responses.ResponseTextConfigVerbosity(v)
		hasFormat = true
	}
	if hasFormat {
		params.Text = text
	}

	if req.Settings != nil && len(req.Settings.ResponseInclude) > 0 {
		for _, inc := range req.Settings.ResponseInclude {
			params.Include = append(params.Include, responses.ResponseIncludable(inc))
		}
	}
	// top_logprobs only takes effect when logprobs are included in the output,
	// matching the Python SDK's implicit include.
	if req.Settings != nil && req.Settings.TopLogprobs != nil {
		const logprobsInclude = responses.ResponseIncludable("message.output_text.logprobs")
		if !slices.Contains(params.Include, logprobsInclude) {
			params.Include = append(params.Include, logprobsInclude)
		}
	}

	applySettings(&params, req.Settings, len(tools) > 0)
	return params, nil
}

// requestOptions builds per-request openai-go options from the model settings'
// extra headers, query parameters and body fields.
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
		// WithJSONSet interprets the key as an sjson path, so escape its
		// special characters to set a literal top-level key (Python's
		// extra_body semantics).
		opts = append(opts, option.WithJSONSet(escapeJSONPath(k), v))
	}
	return opts
}

// escapeJSONPath escapes sjson path metacharacters in a literal key.
func escapeJSONPath(k string) string {
	var b strings.Builder
	for _, r := range k {
		switch r {
		case '.', '*', '?', '|', '#', '@', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// GetResponse implements agents.Model.
func (m *ResponsesModel) GetResponse(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.New(ctx, params, requestOptions(req.Settings)...)
	if err != nil {
		return nil, fmt.Errorf("openai responses: %w", err)
	}
	return &agents.ModelResponse{
		Output:     resp.Output,
		Usage:      usageFromResponse(&resp.Usage),
		ResponseID: resp.ID,
	}, nil
}

// StreamResponse implements agents.Model.
func (m *ResponsesModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.TResponseStreamEvent, error] {
	return func(yield func(*agents.TResponseStreamEvent, error) bool) {
		params, err := m.buildParams(req)
		if err != nil {
			yield(nil, err)
			return
		}
		stream := m.client.NewStreaming(ctx, params, requestOptions(req.Settings)...)
		defer stream.Close()
		for stream.Next() {
			event := stream.Current()
			if !yield(&event, nil) {
				return
			}
			// The Responses API reports terminal failures as ordinary stream
			// events that never trip the SSE layer's Err(); surface them as
			// errors so a failed run cannot end as an empty success.
			switch event.Type {
			case "error":
				e := event.AsError()
				yield(nil, fmt.Errorf("openai responses stream error: %s (code %q)", e.Message, e.Code))
				return
			case "response.failed":
				e := event.AsResponseFailed().Response.Error
				yield(nil, fmt.Errorf("openai responses stream: response failed: %s (code %q)", e.Message, e.Code))
				return
			case "response.incomplete":
				reason := event.AsResponseIncomplete().Response.IncompleteDetails.Reason
				yield(nil, fmt.Errorf("openai responses stream: response incomplete: %s", reason))
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("openai responses stream: %w", err))
		}
	}
}

func usageFromResponse(u *responses.ResponseUsage) *agents.Usage {
	if u == nil {
		return agents.NewUsage()
	}
	return &agents.Usage{
		Requests:            1,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		TotalTokens:         u.TotalTokens,
		InputTokensDetails:  agents.InputTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens},
		OutputTokensDetails: agents.OutputTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
	}
}

func settingsToolChoice(s *agents.ModelSettings) agents.ToolChoice {
	if s == nil {
		return ""
	}
	return s.ToolChoice
}

func settingsVerbosity(s *agents.ModelSettings) string {
	if s == nil {
		return ""
	}
	return s.Verbosity
}
