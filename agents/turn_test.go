package agents

import (
	"testing"
)

func TestTurnResult_ToolCallNames(t *testing.T) {
	tr := &TurnResult{NewItems: []*RunItem{
		NewModelItem(ItemToolCall, nil, functionCallOutput(t, "alpha", "c1", `{}`)),
		NewModelItem(ItemMessage, nil, messageOutput(t, "hi")),
		NewModelItem(ItemToolCall, nil, functionCallOutput(t, "beta", "c2", `{}`)),
	}}
	got := tr.ToolCallNames()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("ToolCallNames() = %v, want [alpha beta] in call order", got)
	}
}
