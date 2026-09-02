package agents_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// toolCallScript is a two-turn script: a get_time call, then a text answer.
func toolCallScript() *agentstest.FakeModel {
	return agentstest.NewResponseBuilder().
		FunctionCall("get_time", "call-1", "{}").
		NewTurn().
		Text("noon").
		Build()
}

func timeTool(t *testing.T) *agents.Tool {
	t.Helper()
	return agents.NewTool("get_time", "tells the time",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			return "12:00", nil
		})
}

// ItemsPersistedEvent's promise: at the moment it arrives, every item the
// stream showed before it is in the store. Checked live, against the session,
// at each event.
func TestItemsPersistedEventGuaranteesStoredPrefix(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	agent := &agents.Agent{Name: "a", ModelImpl: toolCallScript(), Tools: []*agents.Tool{timeTool(t)}}

	stream, _ := agents.Run(ctx, agent, "time?", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	itemsSeen, persistEvents := 0, 0
	sawToolOutputBeforeTurnSave := false
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		switch e := ev.(type) {
		case *agents.RunItemStreamEvent:
			itemsSeen++
			if e.Name == "tool_output" && persistEvents == 1 {
				sawToolOutputBeforeTurnSave = true
			}
		case *agents.ItemsPersistedEvent:
			persistEvents++
			entries, eerr := sess.Entries(ctx, session.Cursor{})
			if eerr != nil {
				t.Fatal(eerr)
			}
			stored := 0
			for _, en := range entries {
				if en.Kind == session.EntryKindItem && en.Source.Type != agents.SourceUser {
					stored++
				}
			}
			if stored < itemsSeen {
				t.Fatalf("event #%d: stream showed %d items but store holds %d — the event promised a stored prefix", persistEvents, itemsSeen, stored)
			}
		}
	}
	// One save per boundary that left nothing behind: the user-input save
	// ahead of the first model call, the post-tool turn save, the final save.
	if persistEvents != 3 {
		t.Errorf("persist events = %d, want 3 (user input + turn save + final save)", persistEvents)
	}
	if !sawToolOutputBeforeTurnSave {
		t.Error("the turn save's event arrived before the turn's tool_output; the turn save follows tool execution")
	}
}

// A run without a session has nothing to announce.
func TestItemsPersistedEventAbsentWithoutSession(t *testing.T) {
	agent := &agents.Agent{Name: "a", ModelImpl: toolCallScript(), Tools: []*agents.Tool{timeTool(t)}}
	stream, _ := agents.Run(context.Background(), agent, "time?", agents.RunOptions{})
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(*agents.ItemsPersistedEvent); ok {
			t.Fatal("ItemsPersistedEvent on a run without a session")
		}
	}
}

// An interruption's save holds the pending call back, so the stream has shown
// an item the store does not hold — that boundary must stay silent, even
// though a save of the turn's completed part DID happen (checked against the
// store, so removing the everything-landed condition turns this red).
func TestItemsPersistedEventSilentOnInterruption(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	gated := agents.NewTool("drop_table", "dangerous",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			return "gone", nil
		})
	gated.NeedsApproval = true
	model := agentstest.NewResponseBuilder().
		Text("about to drop").
		FunctionCall("drop_table", "call-1", "{}").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{gated}}

	stream, _ := agents.Run(ctx, agent, "drop it", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	var res *agents.RunResult
	items := 0
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		switch e := ev.(type) {
		case *agents.RunItemStreamEvent:
			items++
		case *agents.ItemsPersistedEvent:
			// The user-input save announces itself before any item; the
			// interruption's save must not.
			if items > 0 {
				t.Fatal("ItemsPersistedEvent on a save that held the pending call back")
			}
		case *agents.RunCompletedEvent:
			res = e.Result
		}
	}
	if res == nil || len(res.Interruptions) != 1 {
		t.Fatalf("expected an interrupted run, got %+v", res)
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	sawModelItem := false
	for _, en := range entries {
		if en.Kind == session.EntryKindItem && en.Source.Type == agents.SourceModel {
			sawModelItem = true
		}
	}
	if !sawModelItem {
		t.Fatal("the turn's completed part (the message) should have persisted — otherwise this test is not exercising a partial save")
	}
}
