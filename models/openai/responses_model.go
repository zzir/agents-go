package openai

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

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
		Model: m.model,
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
	if req.Prompt != nil {
		prompt, err := convertPrompt(req.Prompt)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Prompt = prompt
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if tc, ok := convertToolChoice(settingsToolChoice(req.Settings)); ok {
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

	// parallel_tool_calls gating counts function tools only, excluding handoffs,
	// matching the Python SDK (openai_responses.py:746 uses `tools`, not the
	// combined tool+handoff list).
	applySettings(&params, req.Settings, len(req.Tools) > 0)
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
	var httpResp *http.Response
	opts := append(requestOptions(req.Settings), option.WithResponseInto(&httpResp))
	resp, err := m.client.New(ctx, params, opts...)
	if err != nil {
		return nil, fmt.Errorf("openai responses: %w", err)
	}
	// The Responses API omits the usage block for some responses; count it as
	// zero requests in that case (Python: `Usage() if not response.usage`).
	var usage *responses.ResponseUsage
	if resp.JSON.Usage.Valid() {
		usage = &resp.Usage
	}
	var requestID string
	if httpResp != nil {
		requestID = httpResp.Header.Get("X-Request-Id")
	}
	return &agents.ModelResponse{
		Output:     resp.Output,
		Usage:      usageFromResponse(usage),
		ResponseID: resp.ID,
		RequestID:  requestID,
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
			// typed *ModelBehaviorError so a failed run cannot end as an empty
			// success (Python: response_terminal_failure_error /
			// response_error_event_failure_error).
			switch event.Type {
			case "error", "response.error":
				e := event.AsError()
				yield(nil, responseErrorEventFailure(event.Type, e))
				return
			case "response.failed":
				r := event.AsResponseFailed().Response
				yield(nil, responseTerminalFailure(event.Type, string(r.Status), string(r.Error.Code), r.Error.Message, ""))
				return
			case "response.incomplete":
				r := event.AsResponseIncomplete().Response
				yield(nil, responseTerminalFailure(event.Type, string(r.Status), "", "", r.IncompleteDetails.Reason))
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("openai responses stream: %w", err))
		}
	}
}

// responseTerminalFailure builds a *ModelBehaviorError for a response.failed /
// response.incomplete terminal stream event, mirroring Python's
// format_response_terminal_failure.
func responseTerminalFailure(eventType, status, errCode, errMessage, incompleteReason string) *agents.ModelBehaviorError {
	msg := fmt.Sprintf("Responses stream ended with terminal event `%s`.", eventType)
	var details []string
	if status != "" {
		details = append(details, "status="+status)
	}
	if errCode != "" || errMessage != "" {
		e := errCode
		if errMessage != "" {
			if e != "" {
				e += ": "
			}
			e += errMessage
		}
		details = append(details, "error="+e)
	}
	if incompleteReason != "" {
		details = append(details, "incomplete_details="+incompleteReason)
	}
	if len(details) > 0 {
		msg += " " + strings.Join(details, "; ") + "."
	}
	return agents.NewModelBehaviorError("%s", msg)
}

// responseErrorEventFailure builds a *ModelBehaviorError for an error /
// response.error terminal stream event, mirroring Python's
// format_response_error_event.
func responseErrorEventFailure(eventType string, e responses.ResponseErrorEvent) *agents.ModelBehaviorError {
	msg := fmt.Sprintf("Responses stream ended with terminal event `%s`.", eventType)
	var details []string
	if e.Code != "" {
		details = append(details, "code="+e.Code)
	}
	if e.Message != "" {
		details = append(details, "message="+e.Message)
	}
	if e.Param != "" {
		details = append(details, "param="+e.Param)
	}
	if len(details) > 0 {
		msg += " " + strings.Join(details, "; ") + "."
	}
	return agents.NewModelBehaviorError("%s", msg)
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

func settingsVerbosity(s *agents.ModelSettings) agents.Verbosity {
	if s == nil {
		return ""
	}
	return s.Verbosity
}
