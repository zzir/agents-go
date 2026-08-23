package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrCompactionUnavailable marks a session whose agent cannot run a compaction
// pass — no agent bound, compaction disabled, or no usable provider/model. The
// handler maps it to a 400 with the reason.
var ErrCompactionUnavailable = errors.New("compaction unavailable")

// CompactSession runs one forced compaction pass on the session's active
// branch, outside any run — the Context panel's "Compact now" — with the
// session's bound agent's compaction settings. It reuses the run path's own
// construction (same summary-model resolution, same adapter, same guards), so
// the fold is exactly the one the threshold would have fired; Force only
// skips the threshold. Nothing to fold (the kept window already covers the
// history) returns compacted=false and no error.
//
// The busy check is advisory — a run may start between it and the fold — but
// persistCompaction is transactional and refolds the append point, so the race
// costs at most one turn answering over the pre-fold history.
func (r *Runner) CompactSession(ctx context.Context, sessionID string) (compacted bool, beforeItems, afterItems int, err error) {
	sess, err := r.Deps.Sessions.Get(ctx, sessionID)
	if err != nil {
		return false, 0, 0, err // store.ErrNotFound flows through to a 404
	}
	if sess.AgentConfigID == "" {
		return false, 0, 0, fmt.Errorf("%w: the session has no agent bound", ErrCompactionUnavailable)
	}
	ac, err := r.Deps.AgentConfigs.Get(ctx, sess.AgentConfigID)
	if err != nil {
		return false, 0, 0, fmt.Errorf("%w: loading agent config: %w", ErrCompactionUnavailable, err)
	}
	return r.compactSessionAs(ctx, sessionID, ac)
}

// compactSessionAs is CompactSession with the agent named: a workflow step
// folds the transcript with ITS agent's settings, whichever agent the child
// session is bound to (none, before its first run has answered).
func (r *Runner) compactSessionAs(ctx context.Context, sessionID string, ac *store.AgentConfig) (compacted bool, beforeItems, afterItems int, err error) {
	if rid, live := r.hub.ActiveRunForSession(sessionID); live {
		return false, 0, 0, ErrSessionBusy{RunID: rid}
	}
	if !ac.Compaction.Enabled {
		return false, 0, 0, fmt.Errorf("%w: compaction is disabled on agent %q", ErrCompactionUnavailable, ac.Name)
	}
	spec, err := DecodeAgentSpec(ac)
	if err != nil {
		return false, 0, 0, err
	}
	provider, providerType, err := resolveProvider(ctx, r.Deps, ac, spec, r.Deps.Settings.ProxyClient(ctx))
	if err != nil {
		return false, 0, 0, err
	}
	if provider == nil {
		return false, 0, 0, fmt.Errorf("%w: no API key configured for provider %s", ErrCompactionUnavailable, providerType)
	}
	summaryModel, err := summaryModelFor(provider, ac.Compaction, ac.Model)
	if err != nil {
		return false, 0, 0, fmt.Errorf("%w: resolving the summary model: %w", ErrCompactionUnavailable, err)
	}
	if summaryModel == nil {
		return false, 0, 0, fmt.Errorf("%w: provider has no model %q", ErrCompactionUnavailable, ac.Model)
	}
	ref, err := store.RefFor(ctx, r.db, sessionID)
	if err != nil {
		return false, 0, 0, err
	}
	notify := store.CompactionNotifier{OnDone: func(before, after int) {
		compacted, beforeItems, afterItems = true, before, after
	}}
	ca := store.NewCompactionAdapter(store.NewEntryStoreFor(r.db, ref), summaryModel,
		ac.Compaction.Threshold, ac.Compaction.Window, ac.Compaction.Prompt, notify)
	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		return false, 0, 0, err
	}
	return compacted, beforeItems, afterItems, nil
}
