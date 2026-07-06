package sandbox

import (
	"context"
	"testing"

	agents "github.com/zzir/agents-go/agents"
)

// CodeTool forwards a configured NeedsApprovalFunc to the underlying function
// tool, so a caller can gate execution per call from the command in argsJSON.
func TestCodeToolForwardsNeedsApprovalFunc(t *testing.T) {
	var gotArgs string
	tool := CodeTool(NewLocal(), CodeToolConfig{
		NeedsApprovalFunc: func(_ context.Context, _ *agents.RunContext, argsJSON string) (bool, error) {
			gotArgs = argsJSON
			return true, nil
		},
	})
	ft, ok := tool.(*agents.FunctionTool)
	if !ok || ft.NeedsApprovalFunc == nil {
		t.Fatal("CodeTool did not forward NeedsApprovalFunc")
	}
	need, err := ft.NeedsApprovalFunc(context.Background(), nil, `{"cmd":"rm -rf x"}`)
	if err != nil || !need || gotArgs != `{"cmd":"rm -rf x"}` {
		t.Fatalf("forwarded gate not wired: need=%v err=%v args=%q", need, err, gotArgs)
	}

	// Unset → the tool has no gate (nil func), i.e. never approval-gated here.
	plain := CodeTool(NewLocal(), CodeToolConfig{})
	if pf, ok := plain.(*agents.FunctionTool); !ok || pf.NeedsApprovalFunc != nil {
		t.Fatal("unset NeedsApprovalFunc should leave the tool ungated")
	}
}
