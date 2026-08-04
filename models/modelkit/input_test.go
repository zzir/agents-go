package modelkit

import (
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// textContent concatenates the text of all text-bearing parts.
func textContent(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.IsText() {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestParseInputEasyMessages(t *testing.T) {
	items := []agents.TResponseInputItem{
		agents.InputItemsFromText("hi")[0],
		agents.InputItemsFromSystemText("be brief")[0],
		agents.InputItemsFromAssistantText("hello")[0],
	}
	parsed, err := ParseInput(items)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []string{"user", "system", "assistant"}
	wantTexts := []string{"hi", "be brief", "hello"}
	for i, p := range parsed {
		if p.Type != "message" || p.Role != wantRoles[i] {
			t.Fatalf("item %d: type=%q role=%q, want message/%s", i, p.Type, p.Role, wantRoles[i])
		}
		if got := textContent(p.Parts); got != wantTexts[i] {
			t.Fatalf("item %d: text=%q, want %q", i, got, wantTexts[i])
		}
	}
}

func TestParseInputModelOutputRoundTrip(t *testing.T) {
	msg, err := MessageItem("msg_1", "answer")
	if err != nil {
		t.Fatal(err)
	}
	fc, err := FunctionCallItem("fc_1", "call_1", "lookup", `{"q":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ReasoningItem("rs_1", "hmm", "sig")
	if err != nil {
		t.Fatal(err)
	}
	in, err := agents.OutputToInput([]agents.TResponseOutputItem{rs, msg, fc})
	if err != nil {
		t.Fatal(err)
	}
	in = append(in, responses.ResponseInputItemParamOfFunctionCallOutput("call_1", "result"))

	parsed, err := ParseInput(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 4 {
		t.Fatalf("parsed %d items, want 4", len(parsed))
	}
	if parsed[0].Type != "reasoning" || len(parsed[0].ContentTexts) != 1 || parsed[0].ContentTexts[0] != "hmm" || parsed[0].EncryptedContent != "sig" {
		t.Fatalf("reasoning item parsed wrong: %+v", parsed[0])
	}
	if parsed[1].Type != "message" || parsed[1].Role != "assistant" || textContent(parsed[1].Parts) != "answer" {
		t.Fatalf("message item parsed wrong: %+v", parsed[1])
	}
	if parsed[2].Type != "function_call" || parsed[2].CallID != "call_1" || parsed[2].Name != "lookup" || parsed[2].Arguments != `{"q":"x"}` {
		t.Fatalf("function_call parsed wrong: %+v", parsed[2])
	}
	got := parsed[3]
	if got.Type != "function_call_output" || got.CallID != "call_1" {
		t.Fatalf("function_call_output parsed wrong: %+v", got)
	}
	if textContent(got.Output) != "result" {
		t.Fatalf("output text = %q, want result", textContent(got.Output))
	}
}

func TestParseInputMultimodalToolOutput(t *testing.T) {
	item, ok := toolOutputItem(t, "call_9", []agents.ToolOutputContent{
		agents.ToolOutputText{Text: "see image"},
		agents.ToolOutputImage{ImageURL: "data:image/png;base64,AAAA", Detail: agents.DetailLow},
	})
	if !ok {
		t.Fatal("toolOutputContentItem returned false")
	}
	parsed, err := ParseInput([]agents.TResponseInputItem{item})
	if err != nil {
		t.Fatal(err)
	}
	out := parsed[0].Output
	if len(out) != 2 {
		t.Fatalf("parts = %d, want 2", len(out))
	}
	if !out[0].IsText() || out[0].Text != "see image" {
		t.Fatalf("part 0 = %+v", out[0])
	}
	if out[1].Type != "input_image" || out[1].ImageURL != "data:image/png;base64,AAAA" || out[1].Detail != "low" {
		t.Fatalf("part 1 = %+v", out[1])
	}
}

// toolOutputItem builds a multimodal function_call_output through the public
// agents surface (a fake tool run would do the same).
func toolOutputItem(t *testing.T, callID string, content []agents.ToolOutputContent) (agents.TResponseInputItem, bool) {
	t.Helper()
	list := make(responses.ResponseFunctionCallOutputItemListParam, 0, len(content))
	for _, c := range content {
		switch v := c.(type) {
		case agents.ToolOutputText:
			list = append(list, responses.ResponseFunctionCallOutputItemParamOfInputText(v.Text))
		case agents.ToolOutputImage:
			img := responses.ResponseInputImageContentParam{}
			if v.ImageURL != "" {
				img.ImageURL = param.NewOpt(v.ImageURL)
			}
			if v.Detail != "" {
				img.Detail = responses.ResponseInputImageContentDetail(string(v.Detail))
			}
			list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{OfInputImage: &img})
		default:
			t.Fatalf("unsupported content %T", c)
		}
	}
	return responses.ResponseInputItemParamOfFunctionCallOutput(callID, list), true
}

func TestParseInputUnknownTypePassesThrough(t *testing.T) {
	raw := `{"type":"web_search_call","id":"ws_1","status":"completed"}`
	item, err := agents.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInput([]agents.TResponseInputItem{item})
	if err != nil {
		t.Fatal(err)
	}
	if parsed[0].Type != "web_search_call" {
		t.Fatalf("type = %q, want web_search_call", parsed[0].Type)
	}
	if len(parsed[0].Raw) == 0 {
		t.Fatal("raw bytes missing")
	}
}
