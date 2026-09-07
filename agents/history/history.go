// Package history gives a model two read-only tools over its own session's
// log, history_search and history_read, so what compaction folded out of its
// context is one call away (spec §2.5i).
package history

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// Bounds on what one call moves, documented defaults (spec §4).
const (
	MaxLimit      = 20
	ExcerptChars  = 2000
	MaxReadChars  = 20_000
	MaxQueryChars = 1000
)

// Source is what the tools read: a *session.Session, or a host's own view.
type Source interface {
	SearchHistory(ctx context.Context, q session.HistoryQuery) (hits []session.Entry, more bool, err error)
	Entry(ctx context.Context, id string) (*session.Entry, error)
}

// Resolver opens the source a call reads. scope is the call's scope argument:
// "" for the conversation itself, else whatever the host documented through
// Options.ScopeHint (a task id, say). An error reaches the model as the
// tool's failure.
type Resolver func(ctx context.Context, tc *agents.ToolContext, scope string) (Source, error)

// For resolves every call to one session and refuses any scope.
func For(sess *session.Session) Resolver {
	return func(_ context.Context, _ *agents.ToolContext, scope string) (Source, error) {
		if scope != "" {
			return nil, fmt.Errorf("history: this conversation has no scope %q to search", scope)
		}
		return sess, nil
	}
}

// Options tunes the tools' descriptions.
type Options struct {
	// ScopeHint, when set, tells the model what the scope argument may name.
	// Empty documents the argument as unsupported.
	ScopeHint string
}

type searchArgs struct {
	Query    string `json:"query" jsonschema:"Case-insensitive literal text to find in messages, tool calls and tool outputs; empty lists the newest items"`
	Role     string `json:"role" jsonschema:"Keep one role: user, assistant or tool; empty keeps all"`
	ToolName string `json:"tool_name" jsonschema:"Keep the calls of this tool and their outputs; empty keeps all"`
	Limit    int    `json:"limit" jsonschema:"Items to return, at most 20; 0 means 20"`
	Before   string `json:"before" jsonschema:"An id from a previous result: only items older than it, to page further back; empty starts at the newest"`
	Scope    string `json:"scope" jsonschema:"Empty for this conversation; the tool description says what else it may name"`
}

type readArgs struct {
	ID     string `json:"id" jsonschema:"The item id shown by history_search"`
	Offset int    `json:"offset" jsonschema:"Character offset to start at; 0 for the beginning"`
	Limit  int    `json:"limit" jsonschema:"Characters to return, at most 20000; 0 means 20000"`
	Scope  string `json:"scope" jsonschema:"Empty for this conversation; the tool description says what else it may name"`
}

// Tools returns history_search and history_read over what resolve opens.
func Tools(resolve Resolver, opts Options) []*agents.Tool {
	scope := "The scope argument is unsupported here; leave it empty."
	if opts.ScopeHint != "" {
		scope = opts.ScopeHint
	}
	search := agents.NewTool("history_search",
		"Search this conversation's whole history, including turns that compaction folded out of your context. "+
			"Case-insensitive literal substring over messages, tool calls and tool outputs; an empty query lists the newest items. "+
			"Results come newest first, at most 20 per call, each ending with [id: ...] for history_read; pass before=<id> to page further back. "+
			"The turn in progress is not visible until it ends. The user sees the same history in the transcript. "+scope,
		func(ctx context.Context, tc *agents.ToolContext, a searchArgs) (string, error) {
			return runSearch(ctx, tc, resolve, a)
		})
	search.ReadOnly = true
	read := agents.NewTool("history_read",
		"Read one history item in full by the id history_search showed, as a character window (offset, limit). "+scope,
		func(ctx context.Context, tc *agents.ToolContext, a readArgs) (string, error) {
			return runRead(ctx, tc, resolve, a)
		})
	read.ReadOnly = true
	return []*agents.Tool{search, read}
}

func runSearch(ctx context.Context, tc *agents.ToolContext, resolve Resolver, a searchArgs) (string, error) {
	if len([]rune(a.Query)) > MaxQueryChars {
		return fmt.Sprintf("The query is longer than %d characters; search for a shorter piece of it.", MaxQueryChars), nil
	}
	limit := a.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = MaxLimit
	}
	src, err := resolve(ctx, tc, a.Scope)
	if err != nil {
		return "", err
	}
	hits, more, err := src.SearchHistory(ctx, session.HistoryQuery{
		Query: a.Query, Role: a.Role, ToolName: a.ToolName, Limit: limit, Before: a.Before,
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		if a.Before != "" {
			return "No older items match; check that before names an id from a previous result.", nil
		}
		return "No history items match.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d items, newest first.\n", len(hits))
	for _, e := range hits {
		b.WriteString("\n")
		b.WriteString(header(e))
		b.WriteString("\n")
		text := []rune(session.RenderItem(e.Item))
		if len(text) > ExcerptChars {
			b.WriteString(string(text[:ExcerptChars]))
			fmt.Fprintf(&b, "... (%d more characters; history_read shows the rest)\n", len(text)-ExcerptChars)
		} else {
			b.WriteString(string(text))
			b.WriteString("\n")
		}
	}
	if more {
		fmt.Fprintf(&b, "\nMore before this: pass before=%q.\n", hits[len(hits)-1].ID)
	}
	return b.String(), nil
}

func runRead(ctx context.Context, tc *agents.ToolContext, resolve Resolver, a readArgs) (string, error) {
	src, err := resolve(ctx, tc, a.Scope)
	if err != nil {
		return "", err
	}
	if a.ID == "" {
		return "Pass the id an item showed in history_search.", nil
	}
	e, err := src.Entry(ctx, a.ID)
	if err != nil {
		return "", err
	}
	if e == nil {
		return fmt.Sprintf("No history item has the id %q.", a.ID), nil
	}
	text := []rune(session.RenderItem(e.Item))
	if len(text) == 0 {
		return fmt.Sprintf("Item %s has no readable text.", a.ID), nil
	}
	offset := min(max(a.Offset, 0), len(text))
	limit := a.Limit
	if limit <= 0 || limit > MaxReadChars {
		limit = MaxReadChars
	}
	end := min(offset+limit, len(text))
	var b strings.Builder
	b.WriteString(header(*e))
	b.WriteString("\n")
	b.WriteString(string(text[offset:end]))
	if end < len(text) {
		fmt.Fprintf(&b, "\n(%d more characters after offset %d)", len(text)-end, end)
	}
	return b.String(), nil
}

// header is the one line above an item: its id, what kind of thing it is,
// and when it was written.
func header(e session.Entry) string {
	kind := session.ItemRole(e.Item)
	switch session.ProbeItem(e.Item).Type {
	case "function_call":
		kind = "tool call"
	case "function_call_output":
		kind = "tool output"
	}
	when := ""
	if !e.CreatedAt.IsZero() {
		when = " · " + e.CreatedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("[id: %s] %s%s", e.ID, kind, when)
}
