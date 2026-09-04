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
	"github.com/zzir/agents-go/models/modelkit"
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
	tools := convertTools(req.Tools, req.Handoffs)

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
	settings := modelkit.Settings(req.Settings)
	if tc, ok := convertToolChoice(settings.ToolChoice); ok {
		params.ToolChoice = tc
	}

	text, hasFormat := responseFormat(req.OutputSchema)
	if v := settings.Verbosity; v != "" {
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
	// so the include is added implicitly.
	if req.Settings != nil && req.Settings.TopLogprobs != nil {
		const logprobsInclude = responses.ResponseIncludable("message.output_text.logprobs")
		if !slices.Contains(params.Include, logprobsInclude) {
			params.Include = append(params.Include, logprobsInclude)
		}
	}

	// parallel_tool_calls gating counts function tools only, excluding handoffs:
	// a run whose only "tools" are handoffs has nothing to parallelize.
	applySettings(&params, req.Settings, len(req.Tools) > 0)
	return params, nil
}

// requestOptions builds per-request openai-go options from the model settings'
// extra headers, query parameters and body fields.
func requestOptions(s *agents.ModelSettings) []option.RequestOption {
	return modelkit.ExtraOptions(s, option.WithHeader, option.WithQuery, option.WithJSONSet)
}

// Respond implements agents.Model.
func (m *ResponsesModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
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
	// zero requests in that case.
	var usage *responses.ResponseUsage
	if resp.JSON.Usage.Valid() {
		usage = &resp.Usage
	}
	var requestID string
	if httpResp != nil {
		requestID = httpResp.Header.Get("X-Request-Id")
	}
	// Terminal statuses fail here as they fail the streaming path; incomplete
	// for length is the one recoverable case (spec §2.7e).
	switch resp.Status {
	case responses.ResponseStatusFailed:
		return nil, withUsage(responseTerminalFailure(agents.EventResponseFailed, string(resp.Status),
			string(resp.Error.Code), resp.Error.Message, ""), usage)
	case responses.ResponseStatusIncomplete:
		if resp.IncompleteDetails.Reason != "max_output_tokens" {
			return nil, withUsage(responseTerminalFailure(agents.EventResponseIncomplete, string(resp.Status),
				"", "", resp.IncompleteDetails.Reason), usage)
		}
	}
	return &agents.ModelResponse{
		Output:           resp.Output,
		Usage:            usageFromResponse(usage),
		ResponseID:       resp.ID,
		RequestID:        requestID,
		Status:           string(resp.Status),
		IncompleteReason: resp.IncompleteDetails.Reason,
	}, nil
}

// withUsage attaches the usage a failed response still billed to its error;
// a response without a usage block is returned as is.
func withUsage(err error, usage *responses.ResponseUsage) error {
	if usage == nil {
		return err
	}
	return &modelkit.UsageError{Err: err, Usage: usageFromResponse(usage)}
}

// terminalUsage is the usage block a terminal stream event carries, or nil.
func terminalUsage(r *responses.Response) *responses.ResponseUsage {
	if !r.JSON.Usage.Valid() {
		return nil
	}
	return &r.Usage
}

// StreamResponse implements agents.Model.
func (m *ResponsesModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		params, err := m.buildParams(req)
		if err != nil {
			yield(nil, err)
			return
		}
		stream := m.client.NewStreaming(ctx, params, requestOptions(req.Settings)...)
		defer stream.Close()
		sawTerminal := false
		for stream.Next() {
			event := stream.Current()
			if !yield(&event, nil) {
				return
			}
			// The Responses API reports terminal failures as ordinary stream events that
			// never trip Err(); surface them as *ModelBehaviorError, not an empty success.
			switch event.Type {
			case agents.EventError, agents.EventResponseError:
				e := event.AsError()
				yield(nil, responseErrorEventFailure(event.Type, e))
				return
			case agents.EventResponseFailed:
				r := event.AsResponseFailed().Response
				yield(nil, withUsage(responseTerminalFailure(event.Type, string(r.Status), string(r.Error.Code), r.Error.Message, ""), terminalUsage(&r)))
				return
			case agents.EventResponseIncomplete:
				r := event.AsResponseIncomplete().Response
				if r.IncompleteDetails.Reason == "max_output_tokens" {
					// Not a failure: the response arrived, cut off, and the
					// runner handles that (spec §2.7e).
					return
				}
				yield(nil, withUsage(responseTerminalFailure(event.Type, string(r.Status), "", "", r.IncompleteDetails.Reason), terminalUsage(&r)))
				return
			case agents.EventResponseCompleted:
				sawTerminal = true
			}
		}
		if sawTerminal {
			// A transport error AFTER the terminal event is not surfaced: the
			// response is already complete and delivered.
			return
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("openai responses stream: %w", err))
			return
		}
		// A clean SSE end without a terminal event is a severed connection,
		// surfaced retryably.
		yield(nil, modelkit.TruncatedStreamError("openai responses stream"))
	}
}

// responseTerminalFailure builds a *ModelBehaviorError for a response.failed /
// response.incomplete terminal stream event.
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
// response.error terminal stream event.
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

// usageFromResponse maps a usage block, omitted for some responses — nil counts
// as zero requests; the mapping is shared with streaming (UsageFromResponseUsage).
func usageFromResponse(u *responses.ResponseUsage) *agents.Usage {
	if u == nil {
		return agents.NewUsage()
	}
	return agents.UsageFromResponseUsage(*u)
}
