package agents

import (
	"context"
	"fmt"
)

// ForkSession copies the entire history from src into dst, producing a full
// clone. dst is cleared first so it contains exactly what src had at the time
// of the call.
func ForkSession(ctx context.Context, src, dst Session) error {
	entries, err := src.GetEntries(ctx, 0)
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	if err := dst.Clear(ctx); err != nil {
		return fmt.Errorf("fork: clearing destination session: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if err := dst.AddEntries(ctx, entries); err != nil {
		return fmt.Errorf("fork: writing to destination session: %w", err)
	}
	return nil
}

// ForkSessionAt copies the first n entries from src into dst, producing a
// point-in-time fork. n is clamped to the source length: n <= 0 copies
// nothing, n >= len(entries) copies everything. dst is cleared first.
//
// Choose n on a paired-item boundary: cutting between a function_call and its
// function_call_output leaves a dangling call in dst, and the API rejects such
// a history on the next run. When forking after a tool call, include both the
// call and its output.
func ForkSessionAt(ctx context.Context, src, dst Session, n int) error {
	entries, err := src.GetEntries(ctx, 0)
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	if err := dst.Clear(ctx); err != nil {
		return fmt.Errorf("fork: clearing destination session: %w", err)
	}
	if n <= 0 || len(entries) == 0 {
		return nil
	}
	if n > len(entries) {
		n = len(entries)
	}
	if err := dst.AddEntries(ctx, entries[:n]); err != nil {
		return fmt.Errorf("fork: writing to destination session: %w", err)
	}
	return nil
}

// IndexOfItemID scans items for the first one whose server-assigned ID matches
// id, returning its zero-based index. Only items the model produced carry IDs
// (output messages, function calls, function call outputs, reasoning items);
// user-created "easy" messages have no ID and are never matched.
//
// Use this to convert a server-assigned ID into a fork-point index. Always
// check ok — forking with a not-found index would silently produce an empty
// fork:
//
//	idx, ok := agents.IndexOfItemID(items, "msg_abc123")
//	if !ok {
//		return fmt.Errorf("item %q not in session", "msg_abc123")
//	}
//	err := agents.ForkSessionAt(ctx, src, dst, idx+1) // include the matched item
func IndexOfItemID(items []TResponseInputItem, id string) (int, bool) {
	for i := range items {
		if itemID(&items[i]) == id {
			return i, true
		}
	}
	return -1, false
}

// itemID extracts the server-assigned ID from an input item, or "" if the item
// variant has no ID (e.g. EasyInputMessage).
func itemID(item *TResponseInputItem) string {
	switch {
	case item.OfOutputMessage != nil:
		return item.OfOutputMessage.ID
	case item.OfFunctionCall != nil:
		return item.OfFunctionCall.ID.Or("")
	case item.OfFunctionCallOutput != nil:
		return item.OfFunctionCallOutput.ID.Or("")
	case item.OfReasoning != nil:
		return item.OfReasoning.ID
	case item.OfComputerCall != nil:
		return item.OfComputerCall.ID
	case item.OfComputerCallOutput != nil:
		return item.OfComputerCallOutput.ID.Or("")
	case item.OfFileSearchCall != nil:
		return item.OfFileSearchCall.ID
	case item.OfWebSearchCall != nil:
		return item.OfWebSearchCall.ID
	case item.OfCodeInterpreterCall != nil:
		return item.OfCodeInterpreterCall.ID
	case item.OfMcpCall != nil:
		return item.OfMcpCall.ID
	case item.OfMcpApprovalRequest != nil:
		return item.OfMcpApprovalRequest.ID
	default:
		return ""
	}
}
