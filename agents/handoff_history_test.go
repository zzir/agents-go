package agents

import (
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func userMsg(text string) TResponseInputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser)
}

func asstMsg(text string) TResponseInputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant)
}

func TestNestHandoffHistory_FoldsToSingleMessage(t *testing.T) {
	hist := []TResponseInputItem{userMsg("what is my balance?"), asstMsg("let me check")}
	filter := NestHandoffHistory(NestHistoryOptions{})

	out := filter(HandoffInputData{InputHistory: hist}).InputHistory
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1 folded message", len(out))
	}
	text := inputItemText(out[0])
	for _, want := range []string{defaultHistoryStartMarker, defaultHistoryEndMarker, "what is my balance?", "let me check"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q\n%s", want, text)
		}
	}
}

func TestNestHandoffHistory_FlattensPriorSummary(t *testing.T) {
	filter := NestHandoffHistory(NestHistoryOptions{})

	// First handoff folds the original two turns.
	hist := []TResponseInputItem{userMsg("hi"), asstMsg("hello")}
	folded := filter(HandoffInputData{InputHistory: hist}).InputHistory

	// Second handoff sees the prior summary plus two new turns.
	second := append(append([]TResponseInputItem{}, folded...), userMsg("next"), asstMsg("more"))

	// Flatten must expand the prior summary back to its 2 turns (total 4), not
	// keep it as one opaque summary item.
	flat := flattenNestedHistory(second, defaultHistoryStartMarker, defaultHistoryEndMarker)
	if len(flat) != 4 {
		t.Fatalf("flattened to %d items, want 4 (summary expanded)", len(flat))
	}
	if got := inputItemText(flat[0]); got != "hi" {
		t.Errorf("flat[0] = %q, want hi", got)
	}

	// The re-folded summary contains all four turns and nests the markers only once.
	refolded := filter(HandoffInputData{InputHistory: second}).InputHistory
	if len(refolded) != 1 {
		t.Fatalf("re-folded to %d items, want 1", len(refolded))
	}
	text := inputItemText(refolded[0])
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
		Mapper: func(transcript []TResponseInputItem) []TResponseInputItem {
			called++
			if len(transcript) != 2 {
				t.Errorf("mapper got %d items, want 2", len(transcript))
			}
			return []TResponseInputItem{userMsg("SUMMARY")}
		},
	})
	out := filter(HandoffInputData{InputHistory: []TResponseInputItem{userMsg("a"), asstMsg("b")}}).InputHistory
	if called != 1 {
		t.Errorf("mapper called %d times, want 1", called)
	}
	if len(out) != 1 || inputItemText(out[0]) != "SUMMARY" {
		t.Errorf("output = %v, want one SUMMARY message", out)
	}
}

func TestNestHandoffHistory_CustomMarkers(t *testing.T) {
	filter := NestHandoffHistory(NestHistoryOptions{StartMarker: "<<H>>", EndMarker: "<</H>>"})
	out := filter(HandoffInputData{InputHistory: []TResponseInputItem{userMsg("x")}}).InputHistory
	text := inputItemText(out[0])
	if !strings.Contains(text, "<<H>>") || !strings.Contains(text, "<</H>>") {
		t.Errorf("custom markers missing:\n%s", text)
	}
	if strings.Contains(text, defaultHistoryStartMarker) {
		t.Errorf("default marker leaked:\n%s", text)
	}
}

func TestNestHandoffHistory_EmptyHistory(t *testing.T) {
	filter := NestHandoffHistory(NestHistoryOptions{})
	out := filter(HandoffInputData{InputHistory: nil}).InputHistory
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1", len(out))
	}
	if !strings.Contains(inputItemText(out[0]), "no previous turns recorded") {
		t.Errorf("empty-history summary missing placeholder:\n%s", inputItemText(out[0]))
	}
}
