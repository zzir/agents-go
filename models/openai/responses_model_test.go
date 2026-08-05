package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// fakeSchema is a minimal OutputSchema for exercising response_format building
// before the reflection-based implementation lands in Phase 2.
type fakeSchema struct {
	plain  bool
	strict bool
}

func (f fakeSchema) IsPlainText() bool        { return f.plain }
func (f fakeSchema) Name() string             { return "final_output" }
func (f fakeSchema) IsStrictJSONSchema() bool { return f.strict }
func (f fakeSchema) JSONSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f fakeSchema) ValidateJSON(string) (any, error) { return nil, nil }

func marshalParams(t *testing.T, req agents.ModelRequest) map[string]any {
	t.Helper()
	m := NewResponsesModel("gpt-4o", NewProvider().client.Responses)
	params, err := m.buildParams(req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestBuildParams_BasicTextInput(t *testing.T) {
	req := agents.ModelRequest{
		SystemInstructions: "be helpful",
		Input:              agents.InputItemsFromText("hello"),
	}
	got := marshalParams(t, req)

	if got["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", got["model"])
	}
	if got["instructions"] != "be helpful" {
		t.Errorf("instructions = %v", got["instructions"])
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("tools should be omitted when none provided, got %v", got["tools"])
	}
}

func TestBuildParams_FunctionTool(t *testing.T) {
	tool := &agents.FunctionTool{
		Name:        "get_weather",
		Description: "look up weather",
		Strict:      true,
		ParamsJSONSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"city": map[string]any{"type": "string"}},
			"required":             []any{"city"},
			"additionalProperties": false,
		},
	}
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("weather in SF?"),
		Tools: []agents.Tool{tool},
	}
	got := marshalParams(t, req)

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", got["tools"])
	}
	ft := tools[0].(map[string]any)
	if ft["name"] != "get_weather" {
		t.Errorf("tool name = %v", ft["name"])
	}
	if ft["type"] != "function" {
		t.Errorf("tool type = %v", ft["type"])
	}
	if ft["strict"] != true {
		t.Errorf("tool strict = %v, want true", ft["strict"])
	}
	if ft["description"] != "look up weather" {
		t.Errorf("tool description = %v", ft["description"])
	}
}

func TestBuildParams_StructuredOutput(t *testing.T) {
	req := agents.ModelRequest{
		Input:        agents.InputItemsFromText("extract"),
		OutputSchema: fakeSchema{plain: false, strict: true},
	}
	got := marshalParams(t, req)

	text, ok := got["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config missing: %v", got["text"])
	}
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("format type = %v", format["type"])
	}
	if format["name"] != "final_output" {
		t.Errorf("format name = %v", format["name"])
	}
	if format["strict"] != true {
		t.Errorf("format strict = %v", format["strict"])
	}
}

func TestBuildParams_PlainTextOutputOmitsFormat(t *testing.T) {
	req := agents.ModelRequest{
		Input:        agents.InputItemsFromText("hi"),
		OutputSchema: fakeSchema{plain: true},
	}
	got := marshalParams(t, req)
	if _, ok := got["text"]; ok {
		t.Errorf("text config should be omitted for plain text, got %v", got["text"])
	}
}

func TestBuildParams_Settings(t *testing.T) {
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Tools: []agents.Tool{&agents.FunctionTool{Name: "t", ParamsJSONSchema: map[string]any{"type": "object"}}},
		Settings: &agents.ModelSettings{
			Temperature:       agents.Ptr(0.5),
			MaxTokens:         agents.Ptr[int64](256),
			ToolChoice:        agents.ToolChoiceRequired,
			ParallelToolCalls: agents.Ptr(true),
		},
	}
	got := marshalParams(t, req)

	if got["temperature"] != 0.5 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["max_output_tokens"] != float64(256) {
		t.Errorf("max_output_tokens = %v", got["max_output_tokens"])
	}
	if got["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
	if got["parallel_tool_calls"] != true {
		t.Errorf("parallel_tool_calls = %v", got["parallel_tool_calls"])
	}
}

func TestBuildParams_PromptCacheKey(t *testing.T) {
	req := agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{PromptCacheKey: "my-key"},
	}
	got := marshalParams(t, req)
	if got["prompt_cache_key"] != "my-key" {
		t.Errorf("prompt_cache_key = %v, want my-key", got["prompt_cache_key"])
	}
}

func TestBuildParams_PromptCacheOptions(t *testing.T) {
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{
			PromptCacheOptions: &agents.PromptCacheOptions{Mode: agents.PromptCacheModeExplicit, TTL: "30m"},
		},
	}
	got := marshalParams(t, req)
	opts, ok := got["prompt_cache_options"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_cache_options = %v, want object", got["prompt_cache_options"])
	}
	if opts["mode"] != "explicit" || opts["ttl"] != "30m" {
		t.Errorf("prompt_cache_options = %v, want mode=explicit ttl=30m", opts)
	}
}

func TestBuildParams_ContextManagement(t *testing.T) {
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{
			ContextManagement: []agents.ContextManagement{
				{Type: "compaction", CompactThreshold: agents.Ptr[int64](200000)},
			},
		},
	}
	got := marshalParams(t, req)
	cm, ok := got["context_management"].([]any)
	if !ok || len(cm) != 1 {
		t.Fatalf("context_management = %v, want 1 entry", got["context_management"])
	}
	entry := cm[0].(map[string]any)
	if entry["type"] != "compaction" {
		t.Errorf("type = %v, want compaction", entry["type"])
	}
	if entry["compact_threshold"] != float64(200000) {
		t.Errorf("compact_threshold = %v, want 200000", entry["compact_threshold"])
	}
}

// parallel_tool_calls only applies when function tools are present; handoffs
// alone must not enable it (openai_responses.py:746 gates on `tools`).
func TestBuildParams_ParallelToolCallsExcludesHandoffs(t *testing.T) {
	req := agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Handoffs: []agents.Handoff{{ToolName: "transfer_to_x", InputJSONSchema: map[string]any{"type": "object"}}},
		Settings: &agents.ModelSettings{ParallelToolCalls: agents.Ptr(true)},
	}
	got := marshalParams(t, req)
	if _, ok := got["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls set with only handoffs present: %v", got["parallel_tool_calls"])
	}

	// With a real function tool, the flag is enabled.
	req.Tools = []agents.Tool{&agents.FunctionTool{Name: "t", ParamsJSONSchema: map[string]any{"type": "object"}}}
	got = marshalParams(t, req)
	if got["parallel_tool_calls"] != true {
		t.Errorf("parallel_tool_calls = %v, want true when a function tool is present", got["parallel_tool_calls"])
	}
}

func TestUsageFromResponse_NoUsageBlock(t *testing.T) {
	// A nil usage block (response carried no usage) counts as zero requests.
	u := usageFromResponse(nil)
	if u.Requests != 0 {
		t.Errorf("Requests = %d, want 0 when no usage block", u.Requests)
	}
}

func TestResponseTerminalFailure_TypedError(t *testing.T) {
	err := responseTerminalFailure("response.failed", "failed", "server_error", "boom", "")
	var mbe *agents.ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("error = %T, want *agents.ModelBehaviorError", err)
	}
	msg := err.Error()
	for _, want := range []string{"response.failed", "status=failed", "server_error", "boom"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestResponseErrorEventFailure_TypedError(t *testing.T) {
	err := responseErrorEventFailure("response.error", responses.ResponseErrorEvent{
		Code: "rate_limit", Message: "slow down", Param: "input",
	})
	var mbe *agents.ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("error = %T, want *agents.ModelBehaviorError", err)
	}
	msg := err.Error()
	for _, want := range []string{"response.error", "code=rate_limit", "message=slow down", "param=input"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

type integArgs struct {
	City string `json:"city" jsonschema:"city name"`
}

func TestBuildParams_GeneratedFunctionToolSchema(t *testing.T) {
	tool := agents.NewFunctionTool("get_weather", "look up weather",
		func(ctx context.Context, tc *agents.ToolContext, a integArgs) (string, error) {
			return "", nil
		})
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("weather?"),
		Tools: []agents.Tool{tool},
	}
	got := marshalParams(t, req)
	tools := got["tools"].([]any)
	ft := tools[0].(map[string]any)
	params := ft["parameters"].(map[string]any)
	if params["additionalProperties"] != false {
		t.Errorf("generated schema not strict: %v", params)
	}
	if ft["strict"] != true {
		t.Errorf("strict = %v", ft["strict"])
	}
}

// A stream that ends cleanly without a terminal event was severed at an event
// boundary (an idle gateway timeout sending a clean FIN): the SSE layer sees a
// normal EOF, but no response.completed ever arrived. It must surface as a
// retryable truncation, not a silent end the runner then reports unretryably.
func TestStreamResponseEndsWithoutTerminalEventIsRetryableTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\n"+
			`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`+"\n\n")
	}))
	t.Cleanup(srv.Close)
	provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	model, err := provider.GetModel("gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	sawCreated := false
	var streamErr error
	for ev, serr := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if serr != nil {
			streamErr = serr
			break
		}
		if ev.Type == "response.created" {
			sawCreated = true
		}
	}
	if !sawCreated {
		t.Fatal("the created event must still be delivered before the error")
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want an io.ErrUnexpectedEOF wrap", streamErr)
	}
	if !RetryableError(streamErr) {
		t.Fatal("a truncated stream must classify as retryable")
	}
}

// A transport error AFTER response.completed must not fail the call: the
// response is complete and already delivered, and failing it now would throw
// away a valid result over a connection with nothing left to say (same rule
// as the Anthropic adapter).
func TestStreamResponseTrailingTransportErrorAfterCompletedIsIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\n"+
			`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","status":"completed","output":[]}}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // sever the connection instead of closing cleanly
	}))
	t.Cleanup(srv.Close)
	provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	model, err := provider.GetModel("gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	sawCompleted := false
	var streamErr error
	for ev, serr := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if serr != nil {
			streamErr = serr
			break
		}
		if ev.Type == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("the completed event must be delivered")
	}
	if streamErr != nil {
		t.Fatalf("err = %v, want nil (terminal event already delivered)", streamErr)
	}
}
