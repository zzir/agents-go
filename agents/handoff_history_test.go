package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

func userMsg(text string) InputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser)
}

func asstMsg(text string) InputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant)
}

func TestNestHandoffHistory_FoldsToSingleMessage(t *testing.T) {
	hist := []InputItem{userMsg("what is my balance?"), asstMsg("let me check")}
	filter := NestHandoffHistory(NestHistoryOptions{})

	out := filter(HandoffInputData{InputHistory: hist}).InputHistory
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1 folded message", len(out))
	}
	text := session.ItemText(out[0])
	for _, want := range []string{defaultHistoryStartMarker, defaultHistoryEndMarker, "what is my balance?", "let me check"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q\n%s", want, text)
		}
	}
}

func TestNestHandoffHistory_FlattensPriorSummary(t *testing.T) {
	filter := NestHandoffHistory(NestHistoryOptions{})

	// First handoff folds the original two turns.
	hist := []InputItem{userMsg("hi"), asstMsg("hello")}
	folded := filter(HandoffInputData{InputHistory: hist}).InputHistory

	// Second handoff sees the prior summary plus two new turns.
	second := append(append([]InputItem{}, folded...), userMsg("next"), asstMsg("more"))

	// Flatten must expand the prior summary back to its 2 turns (total 4), not
	// keep it as one opaque summary item.
	flat := flattenNestedHistory(second)
	if len(flat) != 4 {
		t.Fatalf("flattened to %d items, want 4 (summary expanded)", len(flat))
	}
	if got := session.ItemText(flat[0]); got != "hi" {
		t.Errorf("flat[0] = %q, want hi", got)
	}

	// The re-folded summary contains all four turns and nests the markers only once.
	refolded := filter(HandoffInputData{InputHistory: second}).InputHistory
	if len(refolded) != 1 {
		t.Fatalf("re-folded to %d items, want 1", len(refolded))
	}
	text := session.ItemText(refolded[0])
	if n := strings.Count(text, defaultHistoryStartMarker); n != 1 {
		t.Errorf("start marker appears %d times, want 1 (no summary-of-summary)", n)
	}
	for _, want := range []string{"hi", "hello", "next", "more"} {
		if !strings.Contains(text, want) {
			t.Errorf("re-folded summary missing %q\n%s", want, text)
		}
	}
}

func TestNestHandoffHistory_CustomMapper(t *testing.T) {
	called := 0
	filter := NestHandoffHistory(NestHistoryOptions{
		Mapper: func(transcript []InputItem) []InputItem {
			called++
			if len(transcript) != 2 {
				t.Errorf("mapper got %d items, want 2", len(transcript))
			}
			return []InputItem{userMsg("SUMMARY")}
		},
	})
	out := filter(HandoffInputData{InputHistory: []InputItem{userMsg("a"), asstMsg("b")}}).InputHistory
	if called != 1 {
		t.Errorf("mapper called %d times, want 1", called)
	}
	if len(out) != 1 || session.ItemText(out[0]) != "SUMMARY" {
		t.Errorf("output = %v, want one SUMMARY message", out)
	}
}

func TestNestHandoffHistory_EmptyHistory(t *testing.T) {
	filter := NestHandoffHistory(NestHistoryOptions{})
	out := filter(HandoffInputData{InputHistory: nil}).InputHistory
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1", len(out))
	}
	if !strings.Contains(session.ItemText(out[0]), "no previous turns recorded") {
		t.Errorf("empty-history summary missing placeholder:\n%s", session.ItemText(out[0]))
	}
}

// Handoff OnHandoff callback fires and InputFilter trims the next agent's input.
func TestHandoff_OnHandoffAndInputFilter(t *testing.T) {
	var callbackFired bool

	target := &Agent{Name: "target"}
	target.ModelImpl = &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "target done"))}}

	h := HandoffTo(target)
	h.OnHandoff = func(ctx context.Context, rc *RunContext, argsJSON string) error {
		callbackFired = true
		return nil
	}
	h.InputFilter = func(_ HandoffInputData) HandoffInputData {
		// Drop everything, give the target a single fresh message.
		return HandoffInputData{InputHistory: InputItemsFromText("fresh start")}
	}

	src := &Agent{
		Name:      "src",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_target", "c1", `{}`))}},
		Handoffs:  []Handoff{h},
	}

	targetModel := target.ModelImpl.(*fakeModel)
	res, err := RunSync(context.Background(), src, "original long conversation", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !callbackFired {
		t.Error("OnHandoff callback did not fire")
	}
	if res.LastAgent != target {
		t.Errorf("last agent = %v, want target", res.LastAgent.Name)
	}
	// The target agent should have seen the filtered (single-item) input.
	if len(targetModel.lastReq.Input) != 1 {
		t.Errorf("target input not filtered: %d items, want 1", len(targetModel.lastReq.Input))
	}
}
