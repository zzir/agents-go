package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zzir/agents-go/agents"
)

// ChecklistToolName is the tool an agent keeps its checklist through; the
// chat renders its calls as a checklist card.
const ChecklistToolName = "todo_write"

const checklistDescription = "Keep a checklist of the steps of multi-step work. " +
	"Each call REPLACES the whole list, so send every item. " +
	"Keep exactly one item in_progress while you work. " +
	"Do not use it for a task of one or two steps."

// The item states; anything else refuses the whole list.
var checklistStatuses = []string{"pending", "in_progress", "completed"}

// checklistItem is one step as the model sends it.
type checklistItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// decodeChecklist reads todos as an array, or as a string holding one (some
// models send it JSON-encoded) — see decisions §5.99. Absent is the empty list.
func decodeChecklist(raw json.RawMessage) ([]checklistItem, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = json.RawMessage(encoded)
	}
	var todos []checklistItem
	if err := json.Unmarshal(raw, &todos); err != nil {
		return nil, errors.New(`todos must be an array of {"content", "status"} objects`)
	}
	return todos, nil
}

// checklistMarkdown renders the list as a task list: [x] done, [ ] pending,
// [~] in progress.
func checklistMarkdown(todos []checklistItem) string {
	var b strings.Builder
	for _, it := range todos {
		switch it.Status {
		case "completed":
			b.WriteString("- [x] " + it.Content + "\n")
		case "in_progress":
			b.WriteString("- [~] " + it.Content + " (in progress)\n")
		default:
			b.WriteString("- [ ] " + it.Content + "\n")
		}
	}
	return b.String()
}

// checklistTool builds todo_write (decisions §5.82); keep, when set, receives
// each accepted list rendered as markdown.
func checklistTool(keep func(ctx context.Context, markdown string)) *agents.Tool {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "The complete checklist. It replaces the previous one entirely.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "description": "The step, as a short imperative phrase."},
						"status":  map[string]any{"type": "string", "enum": checklistStatuses},
					},
					"required": []string{"content", "status"},
				},
			},
		},
		"required": []string{"todos"},
	}
	tool, err := agents.NewRawTool(ChecklistToolName, checklistDescription, schema,
		func(ctx context.Context, _ *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
			var args struct {
				Todos json.RawMessage `json:"todos"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return agents.ToolResult{}, fmt.Errorf("invalid arguments: %w", err)
			}
			todos, err := decodeChecklist(args.Todos)
			if err != nil {
				return agents.ToolResult{}, err
			}
			counts := map[string]int{}
			for i, it := range todos {
				if strings.TrimSpace(it.Content) == "" {
					return agents.ToolResult{}, fmt.Errorf("item %d has empty content", i)
				}
				if !slices.Contains(checklistStatuses, it.Status) {
					return agents.ToolResult{}, fmt.Errorf("item %d has status %q (want pending, in_progress or completed)", i, it.Status)
				}
				counts[it.Status]++
			}
			if keep != nil {
				keep(ctx, checklistMarkdown(todos))
			}
			return agents.ToolResult{Content: []agents.ToolOutputContent{agents.ToolOutputText{Text: fmt.Sprintf(
				"Checklist updated: %d items (%d completed, %d in progress, %d pending).",
				len(todos), counts["completed"], counts["in_progress"], counts["pending"])}}}, nil
		})
	if err != nil {
		// The schema is this file's own: one strict mode refuses is a bug here.
		panic(err)
	}
	return tool
}
