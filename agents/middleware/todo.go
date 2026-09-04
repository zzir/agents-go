package middleware

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
)

// TodoToolName is the tool a Todo-mode agent maintains its list through.
// Hosts use it to recognize the calls and render the list as a checklist
// instead of a generic tool card.
const TodoToolName = "todo_write"

// TodoStatus is one item's state.
type TodoStatus string

// The three todo states. There is no "cancelled": the list is replaced whole
// on every write, so an abandoned item is simply not in the next list.
const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// TodoItem is one entry of the agent's working list.
type TodoItem struct {
	Content string     `json:"content" jsonschema:"The task, as a short imperative phrase."`
	Status  TodoStatus `json:"status" jsonschema:"pending, in_progress or completed. Empty means pending."`
}

// DefaultTodoInstructions is the todo preamble.
const DefaultTodoInstructions = `Maintain a todo list for MULTI-STEP work with the todo_write tool:
1. Break the task into concrete steps before starting. Skip the list entirely
   when the task is one or two steps — there it is pure overhead.
2. Each todo_write call REPLACES the whole list — always send every item.
3. Mark the step you are working on in_progress (one at a time), and mark
   steps completed as soon as they are done.
Keep the list current; it is how your progress is tracked.`

// Todo has the agent keep a working todo list through a todo_write tool. Each
// call replaces the whole list (spec §2.12). The host observes it through
// OnUpdate or reads the calls off the stream; the middleware renders nothing.
// It rewrites the ENTRY agent only.
type Todo struct {
	// Instructions overrides the todo preamble (empty = DefaultTodoInstructions).
	Instructions string
	// OnUpdate fires after each accepted todo_write with the full current
	// list, which the middleware does not retain. Optional.
	OnUpdate func(ctx context.Context, items []TodoItem)
}

// todoArgs is what the model hands todo_write.
type todoArgs struct {
	Todos []TodoItem `json:"todos" jsonschema:"The complete todo list. This replaces the previous list entirely."`
}

// Run implements agents.RunMiddleware.
func (td Todo) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	if in.Agent == nil {
		return next(ctx, in)
	}
	in.Agent = td.Apply(in.Agent)
	return next(ctx, in)
}

// Apply returns a clone of agent rewritten for todo mode. Run uses it per run;
// a host that rebuilds an agent for a durable resume calls it at build time so
// todo_write dispatches (spec §2.12).
func (td Todo) Apply(agent *agents.Agent) *agents.Agent {
	out := agent.Clone()
	tool := agents.NewTool(TodoToolName,
		"Replace your todo list. Send the COMPLETE list every time — items you omit are dropped.",
		func(ctx context.Context, _ *agents.ToolContext, args todoArgs) (string, error) {
			next := make([]TodoItem, 0, len(args.Todos))
			counts := map[TodoStatus]int{}
			for i, it := range args.Todos {
				if strings.TrimSpace(it.Content) == "" {
					return "", fmt.Errorf("todo %d has empty content", i)
				}
				status := it.Status
				if status == "" {
					status = TodoPending
				}
				switch status {
				case TodoPending, TodoInProgress, TodoCompleted:
				default:
					return "", fmt.Errorf("todo %d has unknown status %q (want pending, in_progress or completed)", i, it.Status)
				}
				next = append(next, TodoItem{Content: it.Content, Status: status})
				counts[status]++
			}
			if td.OnUpdate != nil {
				// next is this call's own slice, so the hook gets a list
				// nothing else holds — no copy needed.
				td.OnUpdate(ctx, next)
			}
			return fmt.Sprintf("Todo list updated: %d items (%d completed, %d in progress, %d pending).",
				len(next), counts[TodoCompleted], counts[TodoInProgress], counts[TodoPending]), nil
		})
	// The clone shares the Tools slice with the original agent; build a fresh
	// slice so the caller's agent is not mutated through the shared array.
	tools := make([]*agents.Tool, 0, len(out.Tools)+1)
	tools = append(tools, out.Tools...)
	tools = append(tools, tool)
	out.Tools = tools
	out.Instructions = agents.WrapInstructions(out.Instructions,
		strings.TrimSpace(firstNonEmpty(td.Instructions, DefaultTodoInstructions)), "")
	return out
}

var _ agents.RunMiddleware = Todo{}
