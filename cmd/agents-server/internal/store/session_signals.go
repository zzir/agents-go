package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// The reads a conversation's status is derived from: each covers the visible
// sessions of an owner (EveryOwner for all), among ids when given.

// OpenApproval is a pending approval with the conversation a person opens to
// answer it: the session it was filed on, or the parent of the task it paused.
type OpenApproval struct {
	RunID     string          `bun:"run_id"`
	SessionID string          `bun:"session_id"`
	Kind      string          `bun:"kind"`
	ToolCalls json.RawMessage `bun:"tool_calls"`
	CreatedAt time.Time       `bun:"created_at"`
	// OpenSessionID is the visible conversation; TaskID and TaskLabel are set
	// for a task's approval.
	OpenSessionID string `bun:"open_session_id"`
	TaskID        string `bun:"task_id"`
	TaskLabel     string `bun:"task_label"`
}

// Calls decodes the approval's pending tool calls, nil on malformed data.
func (a *OpenApproval) Calls() []PendingToolCall {
	p := PendingApproval{ToolCalls: a.ToolCalls}
	return p.ParsedToolCalls()
}

// ofVisibleSessions narrows q to visible sessions (alias) of ownerID, among ids when given.
func ofVisibleSessions(q *bun.SelectQuery, alias, ownerID string, ids []string) *bun.SelectQuery {
	q = q.Where("?.hidden = ?", bun.Ident(alias), false)
	if ownerID != EveryOwner {
		q = q.Where("?.owner_id = ?", bun.Ident(alias), ownerID)
	}
	if ids != nil {
		q = q.Where("?.id IN (?)", bun.Ident(alias), bun.List(ids))
	}
	return q
}

// ListOpen returns the approvals the owner's conversations wait on, oldest
// first: each conversation's own pause, then those of its tasks merged in.
func (s *PendingApprovalStore) ListOpen(ctx context.Context, ownerID string, ids []string) ([]OpenApproval, error) {
	if ownerID == "" {
		return nil, errListNoOwner
	}
	if ids != nil && len(ids) == 0 {
		return nil, nil
	}
	const cols = "pa.run_id, pa.session_id, pa.kind, pa.tool_calls, pa.created_at"
	var own []OpenApproval
	if err := ofVisibleSessions(s.db.NewSelect().Model((*PendingApproval)(nil)).
		ColumnExpr(cols).
		ColumnExpr("pa.session_id AS open_session_id").
		Join("JOIN sessions AS s ON s.id = pa.session_id"), "s", ownerID, ids).
		Scan(ctx, &own); err != nil {
		return nil, fmt.Errorf("listing open approvals: %w", err)
	}
	var ofTasks []OpenApproval
	if err := ofVisibleSessions(s.db.NewSelect().Model((*PendingApproval)(nil)).
		ColumnExpr(cols).
		ColumnExpr("t.parent_session_id AS open_session_id").
		ColumnExpr("t.id AS task_id").
		ColumnExpr("t.label AS task_label").
		Join("JOIN tasks AS t ON t.child_session_id = pa.session_id").
		Join("JOIN sessions AS ps ON ps.id = t.parent_session_id").
		Where(liveParent), "ps", ownerID, ids).
		Scan(ctx, &ofTasks); err != nil {
		return nil, fmt.Errorf("listing open task approvals: %w", err)
	}
	out := slices.Concat(own, ofTasks)
	slices.SortStableFunc(out, func(a, b OpenApproval) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// TaskSignal is one live task's status under its visible parent conversation.
type TaskSignal struct {
	ParentSessionID string `bun:"parent_session_id"`
	Status          string `bun:"status"`
}

// LiveSignals returns the working and input_required tasks of the owner's
// conversations, one row per (conversation, status).
func (s *TaskStore) LiveSignals(ctx context.Context, ownerID string, ids []string) ([]TaskSignal, error) {
	if ownerID == "" {
		return nil, errListNoOwner
	}
	if ids != nil && len(ids) == 0 {
		return nil, nil
	}
	var out []TaskSignal
	if err := ofVisibleSessions(s.db.NewSelect().Model((*Task)(nil)).
		Distinct().
		ColumnExpr("t.parent_session_id, t.status").
		Join("JOIN sessions AS ps ON ps.id = t.parent_session_id").
		Where(liveParent).
		Where("t.status IN (?)", bun.List([]string{taskWorking, taskInputRequired})), "ps", ownerID, ids).
		Scan(ctx, &out); err != nil {
		return nil, fmt.Errorf("listing live task statuses: %w", err)
	}
	return out, nil
}

// EndedInError returns the owner's conversations whose newest run-written
// entry is an error notice: the body is read for annotation rows alone.
func (s *EntryStore) EndedInError(ctx context.Context, ownerID string, ids []string) (map[string]bool, error) {
	if ownerID == "" {
		return nil, errListNoOwner
	}
	if ids != nil && len(ids) == 0 {
		return nil, nil
	}
	last := s.db.NewSelect().TableExpr("entries AS e2").
		Column("e2.id").
		Where("e2.session_id = s.id").Where("e2.gen = s.gen").
		Where("e2.run_id IS NOT NULL").
		OrderExpr("e2.seq DESC").Limit(1)
	tips := ofVisibleSessions(s.db.NewSelect().TableExpr("sessions AS s").
		ColumnExpr("s.id AS session_id").
		ColumnExpr("(?) AS last_id", last), "s", ownerID, ids)
	var rows []struct {
		SessionID string `bun:"session_id"`
		Entry     string `bun:"entry"`
	}
	if err := s.db.NewSelect().TableExpr("(?) AS t", tips).
		ColumnExpr("t.session_id, e.entry").
		Join("JOIN entries AS e ON e.id = t.last_id").
		Where("e.kind = ?", string(session.EntryKindAnnotation)).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("reading the sessions' last entries: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		var e session.Entry
		if json.Unmarshal([]byte(r.Entry), &e) == nil && e.Display != nil && e.Display.Kind == agents.DisplayError {
			out[r.SessionID] = true
		}
	}
	return out, nil
}

// RunsOffPath returns the runs holding an entry off the session's active
// branch — attempts a branch move left behind — from the rows' columns alone.
func (s *EntryStore) RunsOffPath(ctx context.Context, ref session.Ref) (map[string]bool, error) {
	scoped := s.forRef(ref)
	var rows []entryRow
	if err := scoped.scoped(s.db.NewSelect().Model(&rows)).
		Column("entry_id", "parent_id", "run_id").
		OrderExpr("seq ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading the session tree: %w", err)
	}
	at, err := scoped.appendPointIn(ctx, s.db)
	if err != nil {
		return nil, err
	}
	parent := make(map[string]string, len(rows))
	for i := range rows {
		parent[rows[i].EntryID] = rows[i].ParentID
	}
	on := make(map[string]bool, len(rows))
	for id := at.Leaf; id != "" && !on[id]; {
		p, ok := parent[id]
		if !ok {
			break
		}
		on[id] = true
		id = p
	}
	off := map[string]bool{}
	for i := range rows {
		if rows[i].RunID != "" && !on[rows[i].EntryID] {
			off[rows[i].RunID] = true
		}
	}
	return off, nil
}
