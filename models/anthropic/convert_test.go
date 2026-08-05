package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/models/modelkit"
)

func testModel() *MessagesModel {
	return &MessagesModel{model: "claude-test", promptCaching: true}
}

// wireParams builds the request and returns its wire JSON, which is what the
// API would actually see.
func wireParams(t *testing.T, m *MessagesModel, req agents.ModelRequest) map[string]any {
	t.Helper()
	params, err := m.buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestBuildParamsBasics(t *testing.T) {
	wire := wireParams(t, testModel(), agents.ModelRequest{
		SystemInstructions: "be brief",
		Input:              agents.InputItemsFromText("hi"),
	})
	if wire["model"] != "claude-test" {
		t.Errorf("model = %v", wire["model"])
	}
	if wire["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want default %d — the Messages API requires it", wire["max_tokens"], DefaultMaxTokens)
	}
	system := wire["system"].([]any)[0].(map[string]any)
	if system["text"] != "be brief" {
		t.Errorf("system = %v", system)
	}
	if _, ok := wire["cache_control"]; !ok {
		t.Error("cache_control missing — prompt caching defaults on")
	}
	msgs := wire["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

func TestBuildParamsPromptCachingOptOut(t *testing.T) {
	m := testModel()
	m.promptCaching = false
	wire := wireParams(t, m, agents.ModelRequest{Input: agents.InputItemsFromText("hi")})
	if _, ok := wire["cache_control"]; ok {
		t.Error("cache_control present after opting out")
	}
}

// TestBuildParamsMergesAssistantTurn feeds a canonical assistant turn —
// reasoning, message, function_call as separate items, then the tool result —
// and expects one assistant message with thinking/text/tool_use blocks in
// order, followed by one user message with the tool_result.
func TestBuildParamsMergesAssistantTurn(t *testing.T) {
	rs, err := modelkit.ReasoningItem("rs_1", "hmm", signaturePrefix+"sig-1")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := modelkit.MessageItem("msg_1", "calling the tool")
	if err != nil {
		t.Fatal(err)
	}
	fc, err := modelkit.FunctionCallItem("fc_1", "toolu_1", "lookup", `{"q":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	input, err := agents.OutputToInput([]agents.OutputItem{rs, msg, fc})
	if err != nil {
		t.Fatal(err)
	}
	input = append(agents.InputItemsFromText("go"), input...)
	input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput("toolu_1", "result"))

	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	msgs := wire["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, merged assistant, tool-result user): %v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("message 1 role = %v", assistant["role"])
	}
	blocks := assistant["content"].([]any)
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b.(map[string]any)["type"].(string)
	}
	if strings.Join(types, ",") != "thinking,text,tool_use" {
		t.Fatalf("assistant blocks = %v", types)
	}
	thinking := blocks[0].(map[string]any)
	if thinking["signature"] != "sig-1" || thinking["thinking"] != "hmm" {
		t.Errorf("thinking block = %v", thinking)
	}
	toolUse := blocks[2].(map[string]any)
	if toolUse["id"] != "toolu_1" || toolUse["name"] != "lookup" {
		t.Errorf("tool_use block = %v", toolUse)
	}
	if q := toolUse["input"].(map[string]any)["q"]; q != "x" {
		t.Errorf("tool_use input = %v", toolUse["input"])
	}
	result := msgs[2].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("message 2 role = %v", result["role"])
	}
	tr := result["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_result = %v", tr)
	}
}

// A LEADING system message (a compaction summary projected to the front of
// the input) is top-of-conversation content: it must be hoisted into the
// top-level system parameter, keeping messages[0] a conversational turn.
func TestBuildParamsLeadingSystemHoisted(t *testing.T) {
	input := agents.InputItemsFromSystemText("compacted: earlier turns summarized")
	input = append(input, agents.InputItemsFromText("continue")...)
	wire := wireParams(t, testModel(), agents.ModelRequest{
		SystemInstructions: "be brief",
		Input:              input,
	})
	msgs := wire["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %v, want a single leading user turn", msgs)
	}
	system := wire["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system blocks = %d, want instructions + hoisted summary", len(system))
	}
	if got := system[1].(map[string]any)["text"]; got != "compacted: earlier turns summarized" {
		t.Errorf("hoisted system text = %v", got)
	}
}

// A mid-history system message (compaction summary, middleware injection)
// must travel as a mid_conv_system block in a system turn — the Messages API
// has no plain "system" role for input text.
func TestBuildParamsSystemMessageMidHistory(t *testing.T) {
	input := agents.InputItemsFromText("hi")
	input = append(input, agents.InputItemsFromSystemText("conversation was compacted")...)
	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	msgs := wire["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	sys := msgs[1].(map[string]any)
	if role := sys["role"]; role != "system" {
		t.Errorf("mid-history system message role = %v, want system", role)
	}
	block := sys["content"].([]any)[0].(map[string]any)
	if block["type"] != "mid_conv_system" {
		t.Fatalf("system turn block = %v, want mid_conv_system", block)
	}
	inner := block["content"].([]any)[0].(map[string]any)
	if inner["text"] != "conversation was compacted" {
		t.Errorf("mid_conv_system text = %v", inner["text"])
	}
}

func TestBuildParamsReasoningWithoutSignatureIsDropped(t *testing.T) {
	for name, enc := range map[string]string{
		"unsigned":              "",
		"foreign_provider_blob": "gAAAAB-openai-encrypted-reasoning",
	} {
		rs, err := modelkit.ReasoningItem("rs_1", "some text", enc)
		if err != nil {
			t.Fatal(err)
		}
		input, err := agents.OutputToInput([]agents.OutputItem{rs})
		if err != nil {
			t.Fatal(err)
		}
		input = append(agents.InputItemsFromText("hi"), input...)
		wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
		if msgs := wire["messages"].([]any); len(msgs) != 1 {
			t.Fatalf("%s: messages = %d, want 1 — a reasoning item this backend cannot replay must be dropped", name, len(msgs))
		}
	}
}

func TestBuildParamsRedactedThinkingRoundTrip(t *testing.T) {
	rs, err := modelkit.ReasoningItem("rs_1", "", redactedPrefix+"opaque-bytes")
	if err != nil {
		t.Fatal(err)
	}
	input, err := agents.OutputToInput([]agents.OutputItem{rs})
	if err != nil {
		t.Fatal(err)
	}
	input = append(agents.InputItemsFromText("hi"), input...)
	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	block := wire["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["type"] != "redacted_thinking" || block["data"] != "opaque-bytes" {
		t.Errorf("block = %v, want redacted_thinking with original data", block)
	}
}

func TestBuildParamsImageDataURL(t *testing.T) {
	fc, err := modelkit.FunctionCallItem("fc_1", "toolu_1", "screenshot", "{}")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := agents.OutputToInput([]agents.OutputItem{fc})
	if err != nil {
		t.Fatal(err)
	}
	input := agents.InputItemsFromText("hi")
	input = append(input, calls...)
	// A multimodal tool result, through the wire shape ParseInput sees.
	raw := `{"type":"function_call_output","call_id":"toolu_1","output":[{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}`
	result, err := session.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	input = append(input, result)

	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	msgs := wire["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant tool_use, tool-result user)", len(msgs))
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_result = %v", tr)
	}
	img := tr["content"].([]any)[0].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("tool_result content = %v", img)
	}
	source := img["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "QUJD" {
		t.Errorf("image source = %v", source)
	}
}

func TestBuildParamsToolSchemaSurvives(t *testing.T) {
	type args struct {
		City string `json:"city"`
	}
	tool := agents.NewTool("weather", "Get weather.",
		func(_ context.Context, _ *agents.ToolContext, a args) (string, error) { return "", nil })
	wire := wireParams(t, testModel(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Tools: []*agents.Tool{tool},
	})
	tools := wire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	schema := tools[0].(map[string]any)["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("schema type = %v", schema["type"])
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["city"]; !ok {
		t.Errorf("properties = %v", props)
	}
	if req := schema["required"].([]any); len(req) != 1 || req[0] != "city" {
		t.Errorf("required = %v", schema["required"])
	}
	if ap, ok := schema["additionalProperties"]; !ok || ap != false {
		t.Errorf("additionalProperties = %v (strict schemas must survive)", ap)
	}
}

func TestBuildParamsThinkingBudget(t *testing.T) {
	wire := wireParams(t, testModel(), agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium}},
	})
	thinking := wire["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
		t.Errorf("thinking = %v", thinking)
	}
	// budget >= default cap: the default grows instead of failing.
	if wire["max_tokens"] != float64(16384+DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", wire["max_tokens"], 16384+DefaultMaxTokens)
	}
}

func TestBuildParamsThinkingBudgetVsExplicitMaxTokens(t *testing.T) {
	_, err := testModel().buildParams(agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{
			Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium},
			MaxTokens: agents.Ptr(int64(1000)),
		},
	})
	var ue *agents.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UserError for max_tokens below the thinking budget, got %v", err)
	}
}

func TestBuildParamsRejectsUnsupportedSettings(t *testing.T) {
	for name, req := range map[string]agents.ModelRequest{
		"service_tier":         {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{ServiceTier: agents.ServiceTierFlex}},
		"previous_response_id": {Input: agents.InputItemsFromText("hi"), PreviousResponseID: "resp_1"},
		"verbosity":            {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{Verbosity: agents.VerbosityLow}},
		"metadata_other_key":   {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{Metadata: map[string]string{"trace": "x"}}},
	} {
		_, err := testModel().buildParams(req)
		var ue *agents.UserError
		if !errors.As(err, &ue) {
			t.Errorf("%s: expected UserError, got %v", name, err)
		}
	}
}

func TestBuildParamsMetadataUserID(t *testing.T) {
	wire := wireParams(t, testModel(), agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{Metadata: map[string]string{"user_id": "u-1"}},
	})
	md := wire["metadata"].(map[string]any)
	if md["user_id"] != "u-1" {
		t.Errorf("metadata = %v", md)
	}
}

func TestBuildParamsOutputSchema(t *testing.T) {
	type out struct {
		Answer string `json:"answer"`
	}
	schema := agents.OutputType[out]()
	wire := wireParams(t, testModel(), agents.ModelRequest{
		Input:        agents.InputItemsFromText("hi"),
		OutputSchema: schema,
	})
	oc := wire["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("format = %v", format)
	}
	if _, ok := format["schema"].(map[string]any)["properties"]; !ok {
		t.Errorf("schema = %v", format["schema"])
	}
}

// A side-effect tool returning "" is routine; the Messages API rejects empty
// text blocks but accepts an empty tool_result content list.
func TestBuildParamsEmptyToolResult(t *testing.T) {
	fc, err := modelkit.FunctionCallItem("fc_1", "toolu_1", "noop", "{}")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := agents.OutputToInput([]agents.OutputItem{fc})
	if err != nil {
		t.Fatal(err)
	}
	input := agents.InputItemsFromText("hi")
	input = append(input, calls...)
	input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput("toolu_1", ""))

	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	tr := wire["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" {
		t.Fatalf("tool_result = %v", tr)
	}
	if content, ok := tr["content"]; ok {
		if parts, isList := content.([]any); isList && len(parts) > 0 {
			t.Fatalf("empty tool output produced content %v — an empty text block fails the whole next turn", content)
		}
	}
}

func TestBuildParamsThinkingSamplingConflicts(t *testing.T) {
	for name, s := range map[string]*agents.ModelSettings{
		"temperature": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, Temperature: agents.Ptr(0.5)},
		"top_p":       {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, TopP: agents.Ptr(0.9)},
		"tool_choice": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, ToolChoice: agents.ToolChoiceRequired},
	} {
		_, err := testModel().buildParams(agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: s})
		var ue *agents.UserError
		if !errors.As(err, &ue) {
			t.Errorf("%s: expected UserError for thinking conflict, got %v", name, err)
		}
	}
}

func TestContextWindowExceededFeedsOverflowPolicy(t *testing.T) {
	_, _, err := statusFromStopReason("model_context_window_exceeded")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !agents.DetectContextOverflow(err) {
		t.Fatalf("the overflow detector must recognize the error: %v", err)
	}
}
