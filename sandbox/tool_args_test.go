package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agents "github.com/zzir/agents-go/agents"
)

func TestLenientString_UnmarshalJSON(t *testing.T) {
	ok := map[string]lenientString{
		`"build"`: "build",
		`""`:      "",
		`0`:       "0",
		`42`:      "42",
		`1.5`:     "1.5",
		`true`:    "true",
		`false`:   "false",
		`null`:    "",
	}
	for in, want := range ok {
		var got lenientString
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("Unmarshal(%s) error: %v", in, err)
		} else if got != want {
			t.Errorf("Unmarshal(%s) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{`{}`, `{"a":1}`, `[1,2]`} {
		var got lenientString
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%s) = %q, want error", in, got)
		}
	}
}

// A model on a backend that does not enforce strict schemas may fill an unused
// string field with a zero-value sentinel (session_id: 0). That decodes as its
// literal text instead of failing the call.
func TestCodeTool_LenientArgs(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"echo lenient ok","timeout_seconds":0,"workdir":null,"session_id":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError {
		t.Fatalf("IsError = true, output: %v", out.ModelOutput())
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "lenient ok") {
		t.Errorf("output missing stdout: %q", s)
	}
}

// With sessions enabled, a numeric session_id names the session by its literal
// text: 0 and "0" are the same shell, not an error and not two shells.
func TestCodeTool_LenientArgs_NumericSessionName(t *testing.T) {
	sb := &fakeTerminalSandbox{started: make(chan struct{}, 8)}
	tool := CodeTool(sb, CodeToolConfig{Sessions: true})
	ctx := context.Background()
	tc := &agents.ToolContext{}
	for _, argsJSON := range []string{
		`{"cmd":"echo one","timeout_seconds":0,"workdir":"","session_id":0}`,
		`{"cmd":"echo two","timeout_seconds":0,"workdir":"","session_id":"0"}`,
	} {
		out, err := tool.OnInvoke(ctx, tc, argsJSON)
		if err != nil {
			t.Fatal(err)
		}
		if out.IsError {
			t.Fatalf("IsError = true, output: %v", out.ModelOutput())
		}
	}
	if n := sb.opens.Load(); n != 1 {
		t.Errorf("terminals opened = %d, want 1 (0 and \"0\" must share a session)", n)
	}
}

// Arguments that cannot decode at all are refused as model-visible text, like
// a policy veto — not returned as an error, which would abort the run (CodeTool
// sets no FailureErrorFunction).
func TestCodeTool_InvalidArgsRefusedAsText(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":42,"timeout_seconds":0,"workdir":"","session_id":""}`)
	if err != nil {
		t.Fatalf("invalid arguments must feed back as text, got error: %v", err)
	}
	if !out.IsError {
		t.Error("IsError = false, want true")
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "invalid arguments") {
		t.Errorf("output = %q, want it to name invalid arguments", s)
	}
}

// A call OnInvoke will refuse as text must not consume a human approval.
func TestCodeTool_InvalidArgsSkipApproval(t *testing.T) {
	innerCalled := false
	tool := CodeTool(NewLocal(), CodeToolConfig{
		NeedsApprovalFunc: func(context.Context, *agents.RunContext, string, string) (bool, error) {
			innerCalled = true
			return true, nil
		},
	})
	need, err := tool.NeedsApprovalFunc(context.Background(), nil, `{"cmd":42}`, "call1")
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Error("malformed arguments must not request approval")
	}
	if innerCalled {
		t.Error("inner NeedsApprovalFunc must not run for malformed arguments")
	}
}

// The lenient decoding is invisible in the schema: the model is still told
// these fields are plain strings.
func TestCodeTool_LenientArgsSchemaIsString(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{})
	props, _ := tool.ParamsJSONSchema["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("schema has no properties: %v", tool.ParamsJSONSchema)
	}
	for _, field := range []string{"session_id", "workdir"} {
		p, _ := props[field].(map[string]any)
		if p == nil {
			t.Fatalf("schema missing %s: %v", field, props)
		}
		if p["type"] != "string" {
			t.Errorf("schema %s type = %v, want string", field, p["type"])
		}
	}
}
