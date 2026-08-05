package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// Each todo_write replaces the whole list; OnUpdate sees every accepted
// state, statuses defaulting to pending.
func TestTodo_WriteReplacesAndNotifies(t *testing.T) {
	model := &scriptedModel{responses: []*agents.ModelResponse{
		// Strict schemas make every field required; "" is how a model leaves
		// status unset, and it must default to pending.
		resp(toolCallArgs(t, TodoToolName, "c1",
			`{"todos":[{"content":"read the code","status":""},{"content":"fix it","status":"in_progress"}]}`)),
		resp(toolCallArgs(t, TodoToolName, "c2",
			`{"todos":[{"content":"fix it","status":"completed"}]}`)),
		resp(message(t, "done")),
	}}
	var updates [][]TodoItem
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Todo{
			OnUpdate: func(_ context.Context, items []TodoItem) { updates = append(updates, items) },
		}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalOutputString() != "done" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(updates))
	}
	first := updates[0]
	if len(first) != 2 || first[0].Status != TodoPending || first[1].Status != TodoInProgress {
		t.Fatalf("first update = %+v", first)
	}
	second := updates[1]
	if len(second) != 1 || second[0].Content != "fix it" || second[0].Status != TodoCompleted {
		t.Fatalf("second update (whole-list replace) = %+v", second)
	}
}

// A malformed list is refused whole — no partial acceptance, no update.
func TestTodo_InvalidStatusRefusedWhole(t *testing.T) {
	model := &scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, TodoToolName, "c1",
			`{"todos":[{"content":"ok"},{"content":"bad","status":"someday"}]}`)),
		resp(message(t, "done")),
	}}
	var updates int
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	if _, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Todo{
			OnUpdate: func(context.Context, []TodoItem) { updates++ },
		}},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if updates != 0 {
		t.Fatalf("a rejected write must not notify; got %d updates", updates)
	}
}

// The preamble reaches the model's instructions without clobbering the
// agent's own.
func TestTodo_PreambleWrapsInstructions(t *testing.T) {
	agent := &agents.Agent{Name: "a", Instructions: agents.StaticInstructions("be brief")}
	var seen string
	mw := Todo{}
	stream := mw.Run(context.Background(), func(_ context.Context, in agents.RunInput) agents.RunStream {
		prompt, err := in.Agent.Instructions(context.Background(), nil, in.Agent)
		if err != nil {
			t.Fatal(err)
		}
		seen = prompt
		return func(func(agents.StreamEvent, error) bool) {}
	}, agents.RunInput{Agent: agent})
	stream(func(agents.StreamEvent, error) bool { return true })
	if !strings.Contains(seen, "todo_write") || !strings.Contains(seen, "be brief") {
		t.Fatalf("wrapped instructions = %q", seen)
	}
	if len(agent.Tools) != 0 {
		t.Fatal("the caller's agent must not gain the tool")
	}
}
