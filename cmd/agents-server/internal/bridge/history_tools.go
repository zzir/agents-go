package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/history"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// historyTools builds history_search / history_read for a chat run: the
// session is opened per call from the run context, and a scope names one of
// the session's own background tasks.
func (r *Runner) historyTools(_ context.Context, _ string) []*agents.Tool {
	var lookup taskLookup
	if r.Deps.Tasks != nil {
		lookup = r.Deps.Tasks.Get
	}
	resolve := func(ctx context.Context, tc *agents.ToolContext, scope string) (history.Source, error) {
		sid, _ := tc.Context.(string)
		if sid == "" {
			return nil, errors.New("history: no session in the run context")
		}
		return historySource(ctx, r.db, lookup, sid, scope)
	}
	return history.Tools(resolve, history.Options{
		ScopeHint: "scope may be a task_id from spawn_task, to search that task's own conversation instead of this one.",
	})
}

// taskLookup is the one task read the scope check needs.
type taskLookup func(ctx context.Context, id string) (*store.Task, error)

// historySource opens the session a call may read: its own, or the child
// session of a task this session spawned.
func historySource(ctx context.Context, db *bun.DB, lookup taskLookup, sid, scope string) (history.Source, error) {
	target := sid
	if scope != "" {
		if lookup == nil {
			return nil, fmt.Errorf("history: scope %q: background tasks are not available on this server", scope)
		}
		t, err := lookup(ctx, scope)
		if err != nil || t.ParentSessionID != sid {
			return nil, fmt.Errorf("history: scope %q names no task of this conversation", scope)
		}
		target = t.ChildSessionID
	}
	ref, err := store.RefFor(ctx, db, target)
	if err != nil {
		return nil, fmt.Errorf("history: opening the session: %w", err)
	}
	return session.NewSession(store.NewEntryStoreFor(db, ref)), nil
}
