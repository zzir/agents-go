package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/oaiitems"
	"github.com/zzir/agents-go/models/modelkit"
)

func testModel() *MessagesModel {
	return &MessagesModel{model: "claude-test", promptCaching: true}
}

// functionCallOutput is the function_call_output input item answering callID.
func functionCallOutput(callID, output string) agents.InputItem {
	return oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(output)})
}

// budgetModel is testModel with reasoning effort sent as a thinking budget.
func budgetModel() *MessagesModel {
	m := testModel()
	m.budgetThinking = true
	return m
}

// wireParams builds the request and returns its wire JSON, which is what the
// API would actually see.
func wireParams(t *testing.T, m *MessagesModel, req agents.ModelRequest) map[string]any {
	t.Helper()
	params, _, err := m.buildParams(req)
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
	rs, err := modelkit.ReasoningItem("rs_1", "hmm", bindBlob(signaturePrefix, prefixFingerprint("", nil), "sig-1"))
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
	input = append(input, functionCallOutput("toolu_1", "result"))

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
// travels as a system turn of plain text blocks.
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
	if block["type"] != "text" || block["text"] != "conversation was compacted" {
		t.Errorf("system turn block = %v, want a text block", block)
	}
}

// The API takes a mid-history system message only after a user turn, as the
// last turn or before an assistant one; anywhere else the text rides in a user
// turn instead of drawing a 400.
func TestBuildParamsSystemMessageElsewhereRidesInUserTurn(t *testing.T) {
	notice := agents.InputItemsFromSystemText("notice")
	for name, tc := range map[string]struct {
		input     []agents.InputItem
		wantRoles []string
		wantTexts []string // of the turn carrying the notice
	}{
		"after assistant": {
			input:     append(append(agents.InputItemsFromText("hi"), agents.InputItemsFromAssistantText("ok")...), notice...),
			wantRoles: []string{"user", "assistant", "user"},
			wantTexts: []string{"notice"},
		},
		"between two users": {
			input:     append(append(agents.InputItemsFromText("hi"), notice...), agents.InputItemsFromText("more")...),
			wantRoles: []string{"user"},
			wantTexts: []string{"hi", "notice", "more"},
		},
	} {
		wire := wireParams(t, testModel(), agents.ModelRequest{Input: tc.input})
		msgs := wire["messages"].([]any)
		var roles []string
		for _, m := range msgs {
			roles = append(roles, m.(map[string]any)["role"].(string))
		}
		if !slices.Equal(roles, tc.wantRoles) {
			t.Fatalf("%s: roles = %v, want %v", name, roles, tc.wantRoles)
		}
		var texts []string
		for _, b := range msgs[len(msgs)-1].(map[string]any)["content"].([]any) {
			texts = append(texts, b.(map[string]any)["text"].(string))
		}
		if !slices.Equal(texts, tc.wantTexts) {
			t.Errorf("%s: texts = %v, want %v", name, texts, tc.wantTexts)
		}
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
	fp := prefixFingerprint("", nil)
	rs, err := modelkit.ReasoningItem("rs_1", "", bindBlob(redactedPrefix, fp, "opaque-bytes"))
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

// thinkingTurn is one assistant turn of history: a signed thinking block bound
// to the prefix `system` names, then the answer text.
func thinkingTurn(t *testing.T, id, system string, tools []*agents.Tool) []agents.InputItem {
	t.Helper()
	fp := prefixFingerprint(system, convertTools(tools, nil))
	rs, err := modelkit.ReasoningItem("rs_"+id, "thinking "+id, bindBlob(signaturePrefix, fp, "sig-"+id))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := modelkit.MessageItem("msg_"+id, "answer "+id)
	if err != nil {
		t.Fatal(err)
	}
	items, err := agents.OutputToInput([]agents.OutputItem{rs, msg})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// thinkingSigs lists the signatures of the thinking blocks on the wire, in order.
func thinkingSigs(wire map[string]any) []string {
	var sigs []string
	for _, m := range wire["messages"].([]any) {
		for _, b := range m.(map[string]any)["content"].([]any) {
			if block := b.(map[string]any); block["type"] == "thinking" {
				sigs = append(sigs, block["signature"].(string))
			}
		}
	}
	return sigs
}

// Thinking is bound to the system text and tools it was produced under: a
// block bound to another prefix, and every block before it, is left out of
// the replay; the newest run bound to this prefix stays (spec §2.15).
func TestBuildParamsDropsThinkingBoundToAnotherPrefix(t *testing.T) {
	ask := func(system string, turns ...[]agents.InputItem) []string {
		input := agents.InputItemsFromText("hi")
		for _, turn := range turns {
			input = append(input, turn...)
			input = append(input, agents.InputItemsFromText("more")...)
		}
		return thinkingSigs(wireParams(t, testModel(), agents.ModelRequest{SystemInstructions: system, Input: input}))
	}
	a1, a2 := thinkingTurn(t, "a1", "prompt A", nil), thinkingTurn(t, "a2", "prompt A", nil)
	b1 := thinkingTurn(t, "b1", "prompt B", nil)

	// The system unchanged: every block replays.
	if got := ask("prompt A", a1, a2); !slices.Equal(got, []string{"sig-a1", "sig-a2"}) {
		t.Fatalf("unchanged prefix replays %v, want both blocks", got)
	}
	// The system edited: the older blocks go.
	if got := ask("prompt B", a1, a2); len(got) != 0 {
		t.Fatalf("an edited prefix still replays %v", got)
	}
	// A → B → A: only the run after the last foreign block stays.
	if got := ask("prompt A", a1, b1, a2); !slices.Equal(got, []string{"sig-a2"}) {
		t.Fatalf("A→B→A replays %v, want the last A block only", got)
	}
	// A tool added changes the prefix as much as the text does.
	tool := agents.NewTool("ping", "pings", func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "", nil })
	withTool := thinkingTurn(t, "t1", "prompt A", []*agents.Tool{tool})
	input := append(agents.InputItemsFromText("hi"), withTool...)
	input = append(input, agents.InputItemsFromText("more")...)
	if got := thinkingSigs(wireParams(t, testModel(), agents.ModelRequest{SystemInstructions: "prompt A", Input: input})); len(got) != 0 {
		t.Fatalf("a block bound to a tool set replays %v without the tools", got)
	}
	if got := thinkingSigs(wireParams(t, testModel(), agents.ModelRequest{SystemInstructions: "prompt A", Tools: []*agents.Tool{tool}, Input: input})); !slices.Equal(got, []string{"sig-t1"}) {
		t.Fatalf("with its tools the block replays as %v", got)
	}
	// A block from before fingerprints is bound to no prefix this request has.
	legacy, _ := modelkit.ReasoningItem("rs_old", "old", signaturePrefix+"sig-old")
	old, _ := agents.OutputToInput([]agents.OutputItem{legacy})
	input = append(agents.InputItemsFromText("hi"), old...)
	input = append(input, agents.InputItemsFromText("more")...)
	if got := thinkingSigs(wireParams(t, testModel(), agents.ModelRequest{Input: input})); len(got) != 0 {
		t.Fatalf("an unfingerprinted block replays as %v", got)
	}
}

// The fingerprint a produced block carries is the one its own request's
// replay computes, tool order aside.
func TestPrefixFingerprintIsStable(t *testing.T) {
	a := agents.NewTool("a", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "", nil })
	b := agents.NewTool("b", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "", nil })
	ab := prefixFingerprint("s", convertTools([]*agents.Tool{a, b}, nil))
	ba := prefixFingerprint("s", convertTools([]*agents.Tool{b, a}, nil))
	if ab != ba || len(ab) != fingerprintLen {
		t.Fatalf("fingerprints %q / %q should match and be %d characters", ab, ba, fingerprintLen)
	}
	if prefixFingerprint("s", convertTools([]*agents.Tool{a}, nil)) == ab || prefixFingerprint("t", convertTools([]*agents.Tool{a, b}, nil)) == ab {
		t.Fatal("a different tool set or system text must change the fingerprint")
	}
	_, fp, _ := thinkingBlock(modelkit.Item{EncryptedContent: bindBlob(signaturePrefix, ab, "sig")})
	if fp != ab {
		t.Fatalf("stored fingerprint read back as %q, want %q", fp, ab)
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

// A reasoning effort is adaptive thinking plus output_config.effort — the form
// current models take (decisions §5.76). minimal has no wire value and reads
// as low; the default max_tokens leaves the effort room to think in.
func TestEffortMapsToAdaptiveThinking(t *testing.T) {
	for _, tc := range []struct {
		effort    agents.ReasoningEffort
		wire      string
		maxTokens int64
	}{
		{agents.ReasoningEffortMinimal, "low", DefaultMaxTokens},
		{agents.ReasoningEffortLow, "low", DefaultMaxTokens},
		{agents.ReasoningEffortMedium, "medium", 16384 + DefaultMaxTokens},
		{agents.ReasoningEffortHigh, "high", 32768 + DefaultMaxTokens},
		{agents.ReasoningEffortXhigh, "xhigh", 32768 + DefaultMaxTokens},
		{agents.ReasoningEffortMax, "max", 32768 + DefaultMaxTokens},
	} {
		wire := wireParams(t, testModel(), agents.ModelRequest{
			Input:    agents.InputItemsFromText("hi"),
			Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: tc.effort}},
		})
		thinking, _ := wire["thinking"].(map[string]any)
		if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
			t.Errorf("%s: thinking = %v, want adaptive with a summarized display", tc.effort, thinking)
		}
		if _, has := thinking["budget_tokens"]; has {
			t.Errorf("%s: adaptive thinking carries a budget: %v", tc.effort, thinking)
		}
		oc, _ := wire["output_config"].(map[string]any)
		if oc["effort"] != tc.wire {
			t.Errorf("%s: output_config = %v, want effort %q", tc.effort, oc, tc.wire)
		}
		if wire["max_tokens"] != float64(tc.maxTokens) {
			t.Errorf("%s: max_tokens = %v, want %d", tc.effort, wire["max_tokens"], tc.maxTokens)
		}
	}

	// No effort: nothing about thinking is sent, and the model decides.
	wire := wireParams(t, testModel(), agents.ModelRequest{Input: agents.InputItemsFromText("hi")})
	if _, has := wire["thinking"]; has {
		t.Errorf("thinking sent with no effort set: %v", wire["thinking"])
	}
	if _, has := wire["output_config"]; has {
		t.Errorf("output_config sent with no effort set: %v", wire["output_config"])
	}

	// An explicit max_tokens stands as given, and the effort rides beside a
	// structured-output format without displacing it.
	schema, err := agents.NewDynamicOutputSchema("answer", map[string]any{
		"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		"required": []any{"ok"}, "additionalProperties": false,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	wire = wireParams(t, testModel(), agents.ModelRequest{
		Input:        agents.InputItemsFromText("hi"),
		OutputSchema: schema,
		Settings: &agents.ModelSettings{
			Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortHigh},
			MaxTokens: new(int64(1000)),
		},
	})
	oc, _ := wire["output_config"].(map[string]any)
	if oc["effort"] != "high" || oc["format"] == nil {
		t.Errorf("output_config = %v, want the effort beside the format", oc)
	}
	if wire["max_tokens"] != float64(1000) {
		t.Errorf("max_tokens = %v, want the caller's 1000", wire["max_tokens"])
	}

	// Adaptive thinking takes sampling overrides and a forced tool choice.
	for name, s := range map[string]*agents.ModelSettings{
		"temperature": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, Temperature: new(0.5)},
		"tool_choice": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, ToolChoice: agents.ToolChoiceRequired},
	} {
		if _, _, err := testModel().buildParams(agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: s}); err != nil {
			t.Errorf("%s with an adaptive effort: %v, want it sent", name, err)
		}
	}
}

// "none" cannot be promised — some models think whatever the request says —
// so it is refused by name instead of read as "send nothing".
func TestEffortNoneIsUserError(t *testing.T) {
	for name, m := range map[string]*MessagesModel{"adaptive": testModel(), "budget": budgetModel()} {
		_, _, err := m.buildParams(agents.ModelRequest{
			Input:    agents.InputItemsFromText("hi"),
			Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortNone}},
		})
		if _, ok := errors.AsType[*agents.UserError](err); !ok || !strings.Contains(err.Error(), "none") {
			t.Errorf("%s: err = %v, want a UserError naming the effort", name, err)
		}
	}
}

// The opt-in sends an effort as a thinking token budget, the form models
// before adaptive thinking take: no display, no output_config.
func TestBudgetThinkingOptIn(t *testing.T) {
	wire := wireParams(t, budgetModel(), agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium}},
	})
	thinking := wire["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
		t.Errorf("thinking = %v", thinking)
	}
	if _, has := thinking["display"]; has {
		t.Errorf("a thinking budget carries a display: %v", thinking)
	}
	if _, has := wire["output_config"]; has {
		t.Errorf("a thinking budget carries output_config: %v", wire["output_config"])
	}
	// budget >= default cap: the default grows instead of failing.
	if wire["max_tokens"] != float64(16384+DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", wire["max_tokens"], 16384+DefaultMaxTokens)
	}

	// The provider's switch reaches the models it resolves.
	m, err := NewProvider().WithBudgetThinking(true).Model("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if !m.(*MessagesModel).budgetThinking {
		t.Error("WithBudgetThinking(true) did not reach the model")
	}
}

func TestBudgetThinkingVsExplicitMaxTokens(t *testing.T) {
	_, _, err := budgetModel().buildParams(agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{
			Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium},
			MaxTokens: new(int64(1000)),
		},
	})
	if _, ok := errors.AsType[*agents.UserError](err); !ok {
		t.Fatalf("expected UserError for max_tokens below the thinking budget, got %v", err)
	}
}

// A budget has no value past high: the two upper efforts are refused rather
// than quietly read as high.
func TestBudgetModeRejectsXhigh(t *testing.T) {
	for _, effort := range []agents.ReasoningEffort{agents.ReasoningEffortXhigh, agents.ReasoningEffortMax} {
		_, _, err := budgetModel().buildParams(agents.ModelRequest{
			Input:    agents.InputItemsFromText("hi"),
			Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: effort}},
		})
		if _, ok := errors.AsType[*agents.UserError](err); !ok {
			t.Errorf("%s in budget mode: err = %v, want a UserError", effort, err)
		}
	}
}

func TestBuildParamsRejectsUnsupportedSettings(t *testing.T) {
	for name, req := range map[string]agents.ModelRequest{
		"service_tier":         {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{ServiceTier: agents.ServiceTierFlex}},
		"previous_response_id": {Input: agents.InputItemsFromText("hi"), PreviousResponseID: "resp_1"},
		"verbosity":            {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{Verbosity: agents.VerbosityLow}},
		"metadata_other_key":   {Input: agents.InputItemsFromText("hi"), Settings: &agents.ModelSettings{Metadata: map[string]string{"trace": "x"}}},
	} {
		_, _, err := testModel().buildParams(req)
		if _, ok := errors.AsType[*agents.UserError](err); !ok {
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
	input = append(input, functionCallOutput("toolu_1", ""))

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

// A thinking budget's documented conflicts are refused before the call.
func TestBudgetThinkingSamplingConflicts(t *testing.T) {
	for name, s := range map[string]*agents.ModelSettings{
		"temperature": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, Temperature: new(0.5)},
		"top_p":       {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, TopP: new(0.9)},
		"tool_choice": {Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}, ToolChoice: agents.ToolChoiceRequired},
	} {
		_, _, err := budgetModel().buildParams(agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: s})
		if _, ok := errors.AsType[*agents.UserError](err); !ok {
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

// A refusal part in the replayed history is dropped, not sent back as
// assistant text: a refusal is not an answer the model gave (decisions §5.49).
func TestBuildParamsRefusalPartDropped(t *testing.T) {
	refused, err := session.UnmarshalInputItem([]byte(`{"type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help with that."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := agents.InputItemsFromText("do it")
	input = append(input, refused)
	input = append(input, agents.InputItemsFromText("then something else")...)
	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	for _, m := range wire["messages"].([]any) {
		msg := m.(map[string]any)
		if msg["role"] == "assistant" {
			t.Fatalf("the refusal was replayed as an assistant turn: %v", msg)
		}
		for _, b := range msg["content"].([]any) {
			if strings.Contains(b.(map[string]any)["text"].(string), "cannot help") {
				t.Fatalf("the refusal text reached the wire: %v", msg)
			}
		}
	}
}

// Consecutive text blocks are ONE message item with a part each: the runner
// keeps only a turn's last message, so one item per block would drop all but
// the last (decisions §5.49).
func TestRespondMergesConsecutiveTextBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeMessageSSE(t, w, `{
			"id": "msg_m", "type": "message", "role": "assistant", "model": "claude-test",
			"content": [
				{"type": "thinking", "thinking": "hmm", "signature": "sig-1"},
				{"type": "text", "text": "First part. "},
				{"type": "text", "text": "Second part."},
				{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": {}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`)
	}))
	t.Cleanup(srv.Close)
	provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	model, err := provider.Model("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := model.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, len(resp.Output))
	for i, it := range resp.Output {
		types[i] = it.Type
	}
	if strings.Join(types, ",") != "reasoning,message,function_call" {
		t.Fatalf("output types = %v, want one merged message between the reasoning and the call", types)
	}
	msg := resp.Output[1].AsMessage()
	if msg.ID != "msg_m-1" || len(msg.Content) != 2 {
		t.Fatalf("message = id %q with %d parts, want msg_m-1 with 2 parts", msg.ID, len(msg.Content))
	}
	if got := msg.Content[0].AsOutputText().Text + msg.Content[1].AsOutputText().Text; got != "First part. Second part." {
		t.Errorf("merged text = %q", got)
	}
}

// Respond is served from the stream, so a max_tokens the SDK refuses on a
// blocking call goes through.
func TestRespondStreamsLargeMaxTokens(t *testing.T) {
	var streamed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream    bool  `json:"stream"`
			MaxTokens int64 `json:"max_tokens"`
		}
		if err := json.Unmarshal(readBody(t, r), &body); err != nil {
			t.Error(err)
		}
		streamed = body.Stream && body.MaxTokens == 64000
		writeMessageSSE(t, w, `{
			"id": "msg_big", "type": "message", "role": "assistant", "model": "claude-test",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 1}
		}`)
	}))
	t.Cleanup(srv.Close)
	model, err := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key")).Model("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	maxTokens := int64(64000)
	resp, err := model.Respond(context.Background(), agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{MaxTokens: &maxTokens},
	})
	if err != nil {
		t.Fatalf("Respond with max_tokens 64000: %v", err)
	}
	if !streamed {
		t.Error("the request must carry stream:true and the caller's max_tokens")
	}
	if len(resp.Output) != 1 || resp.Output[0].AsMessage().Content[0].AsOutputText().Text != "ok" {
		t.Errorf("output = %+v", resp.Output)
	}
}

// A refusal reaches a blocking caller as the same canonical refusal part the
// streamed path builds.
func TestRespondRefusalFromStream(t *testing.T) {
	model, err := refusalProvider(t).Model("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := model.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output items = %d, want the one refusal message", len(resp.Output))
	}
	parts := resp.Output[0].AsMessage().Content
	if len(parts) != 1 || parts[0].Type != "refusal" || parts[0].AsRefusal().Refusal != "I cannot help with that." {
		t.Fatalf("content = %+v, want one refusal part", parts)
	}
}

// A function_call cut off mid-arguments (spec §2.7e) replays as a tool_use
// with an empty input: the API requires an object, and the runner already
// refused the call, so the model resends it.
func TestBuildParamsPartialArgumentsReplayAsEmptyObject(t *testing.T) {
	fc, err := modelkit.FunctionCallItem("fc_1", "toolu_1", "lookup", `{"query": "wea`)
	if err != nil {
		t.Fatal(err)
	}
	input, err := agents.OutputToInput([]agents.OutputItem{fc})
	if err != nil {
		t.Fatal(err)
	}
	input = append(agents.InputItemsFromText("go"), input...)
	input = append(input, functionCallOutput("toolu_1", "truncated; not run"))

	wire := wireParams(t, testModel(), agents.ModelRequest{Input: input})
	assistant := wire["messages"].([]any)[1].(map[string]any)
	toolUse := assistant["content"].([]any)[0].(map[string]any)
	if toolUse["id"] != "toolu_1" || toolUse["name"] != "lookup" {
		t.Fatalf("tool_use block = %v", toolUse)
	}
	if in, ok := toolUse["input"].(map[string]any); !ok || len(in) != 0 {
		t.Errorf("tool_use input = %v, want an empty object in place of the partial arguments", toolUse["input"])
	}
}
