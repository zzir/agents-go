package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/zzir/agents-go/agents/session"
)

var _ session.HistorySearcher = (*EntryStore)(nil)

// historyPage is how many candidate bodies one read fetches.
const historyPage = 64

// SearchHistory implements session.HistorySearcher over the active branch,
// compacted rows included: the branch is walked without bodies, the query
// narrows the candidates in SQL where the JSON encoding allows, and bodies
// are read newest first until the page is full.
func (s *EntryStore) SearchHistory(ctx context.Context, q session.HistoryQuery) ([]session.Entry, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = session.DefaultHistoryLimit
	}
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		ExcludeColumn("entry").
		OrderExpr("seq ASC").
		Scan(ctx); err != nil {
		return nil, false, fmt.Errorf("history search: loading entries: %w", err)
	}
	onPath, err := s.activeBranchOfRows(ctx, s.ref, rows)
	if err != nil {
		return nil, false, fmt.Errorf("history search: resolving the active branch: %w", err)
	}
	beforeSeq := int64(math.MaxInt64)
	if q.Before != "" {
		found := false
		for i := range rows {
			if rows[i].EntryID == q.Before {
				beforeSeq, found = rows[i].Seq, true
				break
			}
		}
		if !found {
			return nil, false, nil
		}
	}
	var keep map[string]bool
	if q.Query != "" && likeSafe(q.Query) {
		if keep, err = s.rowsContaining(ctx, q.Query); err != nil {
			return nil, false, err
		}
	}
	var cands []entryRow
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Kind != string(session.EntryKindItem) || !onPath[r.EntryID] || r.Seq >= beforeSeq {
			continue
		}
		if keep != nil && !keep[r.ID] {
			continue
		}
		cands = append(cands, r)
	}
	var calls map[string]string
	if q.ToolName != "" {
		if calls, err = s.callToolNames(ctx, q.ToolName); err != nil {
			return nil, false, err
		}
	}

	var hits []session.Entry
	for start := 0; start < len(cands); start += historyPage {
		page := cands[start:min(start+historyPage, len(cands))]
		bodies, err := s.entryBodies(ctx, s.ref, rowIDs(page))
		if err != nil {
			return nil, false, err
		}
		for _, r := range page {
			e, ok := bodies[r.ID]
			if !ok || !session.MatchesHistory(e, q, calls) {
				continue
			}
			if len(hits) == limit {
				return hits, true, nil
			}
			hits = append(hits, e)
		}
	}
	return hits, false, nil
}

// rowsContaining is the row ids whose stored JSON holds query, case-folded
// in SQL. A necessary condition only: the caller still matches the rendered text.
func (s *EntryStore) rowsContaining(ctx context.Context, query string) (map[string]bool, error) {
	var ids []string
	if err := s.scoped(s.db.NewSelect().Model((*entryRow)(nil))).
		Column("id").
		Where("kind = ?", string(session.EntryKindItem)).
		Where("lower(entry) LIKE ? ESCAPE '\\'", "%"+escapeLike(strings.ToLower(query))+"%").
		Scan(ctx, &ids); err != nil {
		return nil, fmt.Errorf("history search: narrowing by query: %w", err)
	}
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	return keep, nil
}

// callToolNames maps call id to tool name for every function call of tool in
// the session, so an output can be matched to the tool that produced it.
func (s *EntryStore) callToolNames(ctx context.Context, tool string) (map[string]string, error) {
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		Column("id", "entry").
		Where("kind = ?", string(session.EntryKindItem)).
		Where("entry LIKE ? ESCAPE '\\'", `%"name":"`+escapeLike(tool)+`"%`).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("history search: finding calls of %q: %w", tool, err)
	}
	var entries []session.Entry
	for i := range rows {
		var e session.Entry
		if json.Unmarshal([]byte(rows[i].Entry), &e) == nil {
			entries = append(entries, e)
		}
	}
	return session.CallToolNames(entries), nil
}

// likeSafe reports whether a query survives the JSON encoding of the stored
// entry unchanged, so a SQL LIKE over the text is a sound narrowing: ASCII,
// printable, and none of the characters encoding/json escapes.
func likeSafe(query string) bool {
	for _, r := range query {
		if r > unicode.MaxASCII || r < ' ' || r == unicode.MaxASCII || strings.ContainsRune(`"\<>&`, r) {
			return false
		}
	}
	return true
}

// escapeLike quotes LIKE's wildcards and the escape character itself.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
