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
		// special characters to set a literal top-level key.
		opts = append(opts, option.WithJSONSet(modelkit.EscapeJSONPath(k), v))
	}
	return opts
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
	// Terminal statuses fail here exactly as they fail the streaming path: the
	// same response must not be a hard failure when streamed and a silent
	// partial answer when not. Incomplete-for-length is the one recoverable
	// case (the runner refuses its tool calls and the model resends); every
	// other incomplete reason, and a failed response, is terminal.
	switch resp.Status {
	case responses.ResponseStatusFailed:
		return nil, responseTerminalFailure(agents.EventResponseFailed, string(resp.Status),
			string(resp.Error.Code), resp.Error.Message, "")
	case responses.ResponseStatusIncomplete:
		if resp.IncompleteDetails.Reason != "max_output_tokens" {
			return nil, responseTerminalFailure(agents.EventResponseIncomplete, string(resp.Status),
				"", "", resp.IncompleteDetails.Reason)
		}
	}
	return &agents.ModelResponse{
		Output:     resp.Output,
		Usage:      usageFromResponse(usage),
		ResponseID: resp.ID,
		RequestID:  requestID,
		// The blocking path used to drop these while the streaming path read
		// them, so the same truncated response was a hard failure when streamed
		// and a silent partial answer when not.
		Status:           string(resp.Status),
		IncompleteReason: resp.IncompleteDetails.Reason,
	}, nil
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
			// The Responses API reports terminal failures as ordinary stream
			// events that never trip the SSE layer's Err(); surface them as
			// typed *ModelBehaviorError so a failed run cannot end as an empty
			// success.
			switch event.Type {
			case agents.EventError, agents.EventResponseError:
				e := event.AsError()
				yield(nil, responseErrorEventFailure(event.Type, e))
				return
			case agents.EventResponseFailed:
				r := event.AsResponseFailed().Response
				yield(nil, responseTerminalFailure(event.Type, string(r.Status), string(r.Error.Code), r.Error.Message, ""))
				return
			case agents.EventResponseIncomplete:
				r := event.AsResponseIncomplete().Response
				if r.IncompleteDetails.Reason == "max_output_tokens" {
					// Not a failure: the response arrived, it is just cut off.
					// The runner refuses to execute its tool calls and tells
					// the model to resend, which is recoverable — failing the
					// run here would throw away a turn's work over a length
					// limit. Every other incomplete reason still fails.
					return
				}
				yield(nil, responseTerminalFailure(event.Type, string(r.Status), "", "", r.IncompleteDetails.Reason))
				return
			case agents.EventResponseCompleted:
				sawTerminal = true
			}
		}
		if sawTerminal {
			// A transport error AFTER the terminal event is not surfaced
			// (same rule as the Anthropic adapter): the response is already
			// complete and delivered, and failing the call now would throw it
			// away over a connection that had nothing left to say.
			return
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("openai responses stream: %w", err))
			return
		}
		// The SSE layer reports a clean end: a connection severed at an
		// event boundary looks like a normal EOF, but no terminal event
		// ever arrived — the response was cut off. Surfaced retryably
		// (modelkit.TruncatedStreamError wraps io.ErrUnexpectedEOF) so a
		// retry decorator can run the request again.
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

// usageFromResponse maps a blocking response's usage block, which the
// Responses API omits for some responses — nil then counts as zero requests,
// the same rule the streaming path applies through the same
// resp.JSON.Usage.Valid() predicate at the call site above. The mapping itself
// is shared (agents.UsageFromResponseUsage) so the two paths cannot report
// different numbers for the same response.
func usageFromResponse(u *responses.ResponseUsage) *agents.Usage {
	if u == nil {
		return agents.NewUsage()
	}
	return agents.UsageFromResponseUsage(*u)
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
