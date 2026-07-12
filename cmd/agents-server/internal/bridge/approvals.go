package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// persistInterruption serializes an interrupted run's SDK state and its
// pending tool calls to the store, so the approval survives a restart and can
// be resumed from any connection. Best-effort: a persistence failure is
// logged by the caller, and the in-memory hub still holds the live run.
func (r *Runner) persistInterruption(result *RunResult) error {
	if r.Deps.PendingApprovals == nil || result == nil || !result.Interrupted || result.SDKState == nil {
		return nil
	}
	stateJSON, err := result.SDKState.MarshalJSON()
	if err != nil {
		return fmt.Errorf("serializing run state: %w", err)
	}
	calls := make([]store.PendingToolCall, 0, len(result.Interruptions))
	for _, item := range result.Interruptions {
		calls = append(calls, store.PendingToolCall{
			ToolCallID: item.CallID,
			ToolName:   item.ToolName,
			Arguments:  item.Arguments,
		})
	}
	callsJSON, _ := json.Marshal(calls)
	return r.Deps.PendingApprovals.Save(context.Background(), &store.PendingApproval{
		RunID:         result.RunID,
		SessionID:     result.SessionID,
		AgentConfigID: result.AgentConfigID,
		SandboxID:     result.SandboxID,
		State:         string(stateJSON),
		ToolCalls:     callsJSON,
		UserInput:     userInputText(result.SDKState.UserInput),
	})
}

// userInputText renders the user-authored text of a paused turn's new input so
// the UI can rebuild the user bubble on reload. It reuses the same item→role/
// content extraction the messages table uses, so the reconstructed bubble is
// byte-identical to the one the SDK persists once the turn completes.
func userInputText(items []agents.TResponseInputItem) string {
	raw, err := agents.MarshalItems(items)
	if err != nil {
		return ""
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return ""
	}
	var parts []string
	for _, it := range arr {
		m := store.NewItemMessageRaw("", "", "", it)
		if m.Role == "user" {
			if txt := strings.TrimSpace(m.Content); txt != "" {
				parts = append(parts, txt)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// buildAgentRegistry builds the agent from its config and returns a name→agent
// registry covering it and all reachable handoff targets, as required by
// agents.RunStateFromJSON. It must build with the run's sandboxID: the
// restored state's CurrentAgent is resolved FROM this registry and is the very
// agent the SDK re-runs, so omitting the sandbox here strips its
// sandbox-backed tools (exec_command, read_file, …) and the approved call
// fails with "tool not found on agent".
func (r *Runner) buildAgentRegistry(ctx context.Context, agentConfigID, sandboxID string, taskRun bool) (map[string]*agents.Agent, error) {
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, taskRun)
	if err != nil {
		return nil, err
	}
	registry := map[string]*agents.Agent{}
	var walk func(a *agents.Agent)
	walk = func(a *agents.Agent) {
		if a == nil || registry[a.Name] != nil {
			return
		}
		registry[a.Name] = a
		for _, ho := range a.Handoffs {
			if ho.OnInvoke == nil {
				continue
			}
			// The bridge builds every handoff via agents.HandoffTo, whose
			// OnInvoke returns the target regardless of context/args.
			target, err := ho.OnInvoke(ctx, nil, "")
			if err == nil {
				walk(target)
			}
		}
	}
	walk(built.Agent)
	return registry, nil
}

// ResolveApproval applies an approve/reject decision to the pending tool call
// and launches the run's continuation under the same run id. It loads the
// persisted RunState (so it works after a restart and from any transport),
// deletes the pending record, and resumes via the hub. onDone fires when the
// continuation terminates (e.g. to persist a further interruption).
// It also returns the paused session's id (whenever the pending row was loaded,
// even on a later error) so a failed decision stays attributable to its session.
func (r *Runner) ResolveApproval(ctx context.Context, toolCallID string, approve bool, scope ApprovalScope, reason string, onDone func(*RunResult)) (runID, sessionID string, err error) {
	if r.Deps.PendingApprovals == nil {
		return "", "", errors.New("approvals are not persisted")
	}
	pending, _, err := r.Deps.PendingApprovals.FindByToolCall(ctx, toolCallID)
	if err != nil {
		return "", "", err
	}

	// A RunState written by an older server binary can never be resumed — its
	// schema version no longer matches, and RunStateFromJSON enforces strict
	// equality. Detect that up front so it surfaces as a clear, actionable error
	// instead of a masked 500, and discard the stale row so it stops wedging the
	// session (a masked 500 on every approve/reject retry, row never deleted).
	if v := pendingStateSchemaVersion(pending.State); v != agents.RunStateSchemaVersion {
		if delErr := r.Deps.PendingApprovals.Delete(ctx, pending.RunID); delErr != nil {
			zerolog.Ctx(ctx).Error().Err(delErr).Str("run_id", pending.RunID).Msg("discarding stale pending approval")
		}
		return "", pending.SessionID, &StaleApprovalStateError{RunID: pending.RunID, HaveVersion: v, WantVersion: agents.RunStateSchemaVersion}
	}

	// A pending approval may belong to a background task's child session — its
	// agent must be rebuilt task-shaped (no task tools), like the original run.
	registry, err := r.buildAgentRegistry(ctx, pending.AgentConfigID, pending.SandboxID, r.taskMeta(ctx, pending.SessionID) != nil)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("rebuilding agent: %w", err)
	}
	state, err := agents.RunStateFromJSON([]byte(pending.State), registry)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("restoring run state: %w", err)
	}

	item := findApprovalItem(state, toolCallID)
	if item == nil {
		return "", pending.SessionID, fmt.Errorf("tool call %s not found in run state", toolCallID)
	}
	if approve {
		state.Approve(item, false)
		// A task's exec_command gate reads the PARENT session's trust store
		// (trustSessionID); record the grant under the same key or it would
		// never be consulted again.
		r.applyCommandTrust(scope, item, trustSessionID(pending.SessionID, r.taskMeta(ctx, pending.SessionID)))
	} else {
		state.Reject(item, false, reason)
	}

	// Deleting the record is the exclusive claim on this approval: Delete
	// reports ErrNotFound when the row is already gone, so of two concurrent
	// decisions exactly one proceeds. It also has to happen before resuming —
	// the continuation may itself interrupt and persist a fresh record.
	if err := r.Deps.PendingApprovals.Delete(ctx, pending.RunID); err != nil {
		return "", pending.SessionID, fmt.Errorf("claiming pending approval: %w", err)
	}

	// The continuation reopens the SAME run id, so the whole turn — both the
	// interrupted and resumed halves — shares one event stream and trace group.
	runID, err = r.ResumeRun(pending.RunID, state, pending.SessionID, pending.AgentConfigID, pending.SandboxID, onDone)
	if err != nil {
		// Give the approval back (e.g. the session has a live run right now)
		// so the decision can be retried once the session frees up — losing
		// the row here would strand the paused run forever.
		if saveErr := r.Deps.PendingApprovals.Save(context.Background(), pending); saveErr != nil {
			zerolog.Ctx(ctx).Error().Err(saveErr).Str("run_id", pending.RunID).
				Msg("restoring pending approval after failed resume")
		}
		return "", pending.SessionID, err
	}
	return runID, pending.SessionID, nil
}

// StaleApprovalStateError is returned when a persisted RunState cannot be
// resumed because it was written by an older server binary — its schema version
// no longer matches the current one. The stale record is discarded before this
// is returned, so the caller should re-initiate the run rather than retry.
type StaleApprovalStateError struct {
	RunID       string
	HaveVersion string
	WantVersion string
}

func (e *StaleApprovalStateError) Error() string {
	have := e.HaveVersion
	if have == "" {
		have = "unknown"
	}
	return fmt.Sprintf("paused run %s predates the current server version (state schema %s, want %s) and cannot be resumed — re-initiate the run",
		e.RunID, have, e.WantVersion)
}

// pendingStateSchemaVersion reads just the schema_version field of a serialized
// RunState, so a version mismatch is detected without a full (failing) decode.
func pendingStateSchemaVersion(stateJSON string) string {
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	_ = json.Unmarshal([]byte(stateJSON), &probe)
	return probe.SchemaVersion
}

// findApprovalItem returns the interruption in state matching callID, or nil.
func findApprovalItem(state *agents.RunState, callID string) *agents.ToolApprovalItem {
	for _, item := range state.Interruptions {
		if item.CallID == callID {
			return item
		}
	}
	return nil
}

// ApprovalScope controls how far an approve decision extends for exec_command:
// once = just this call; same = trust this exact command for the rest of the
// session; all = trust every command for the session. Ignored for other tools.
type ApprovalScope string

// Approval scopes for ResolveApproval — how far an approve decision extends.
const (
	ApprovalOnce        ApprovalScope = "once"
	ApprovalSameCommand ApprovalScope = "same"
	ApprovalAll         ApprovalScope = "all"
)

// ParseApprovalScope maps a client scope string to an ApprovalScope, defaulting
// to once for an empty or unknown value.
func ParseApprovalScope(s string) ApprovalScope {
	switch ApprovalScope(s) {
	case ApprovalSameCommand:
		return ApprovalSameCommand
	case ApprovalAll:
		return ApprovalAll
	default:
		return ApprovalOnce
	}
}

// execCommandToolName is the fixed name of the sandbox shell tool whose
// executions carry per-session command-trust grants.
const execCommandToolName = "exec_command"

// applyCommandTrust records a session-level exec_command grant per the approval
// scope. It is a no-op for non-exec_command tools, an empty session, or the
// once scope.
func (r *Runner) applyCommandTrust(scope ApprovalScope, item *agents.ToolApprovalItem, sessionID string) {
	if item.ToolName != execCommandToolName || sessionID == "" || r.Deps.SandboxManager == nil {
		return
	}
	trust := r.Deps.SandboxManager.Trust().forSession(sessionID)
	switch scope {
	case ApprovalSameCommand:
		trust.allowCommand(commandHash(item.Arguments))
	case ApprovalAll:
		trust.allowAll()
	}
}
