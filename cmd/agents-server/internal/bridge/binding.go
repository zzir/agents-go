package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrBindingContention reports a first-run bind that lost its validation race
// repeatedly: every attempt re-validated the project against the
// sandbox config's then-current revision, and the config had moved again by
// the time the bind's CAS landed. Transient by construction — the config is
// being actively edited; the client retries once it settles. Handlers map it
// to 409.
var ErrBindingContention = errors.New("sandbox configuration keeps changing; try again")

// ErrInvalidBinding refuses a first-run sandbox binding whose values could
// never work: an unknown sandbox id, or a project that is not the session
// owner's on that sandbox. Refused at bind time, before anything is written —
// the binding is permanent, so a bad value accepted here would disable the
// session's sandbox for good. Handlers map it to 400.
type ErrInvalidBinding struct{ Reason string }

func (e ErrInvalidBinding) Error() string { return "invalid sandbox binding: " + e.Reason }

// resolveBindingProject resolves the project a session is about to be bound
// to: the named one — which must exist, belong to the session's owner and
// live on the named sandbox — or, unnamed, the owner's default project on
// that sandbox, created on first use (decisions §5.28).
func (r *Runner) resolveBindingProject(ctx context.Context, ownerID, sandboxID, projectID string) (*store.Project, error) {
	if r.Deps.Projects == nil {
		return nil, fmt.Errorf("no project store is wired")
	}
	if projectID == "" {
		return r.Deps.Projects.EnsureDefault(ctx, ownerID, sandboxID)
	}
	proj, err := r.Deps.Projects.Get(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidBinding{Reason: "project not found: " + projectID}
		}
		return nil, err
	}
	// A foreign project reads as absent — ownership is not an oracle for
	// existence (the authz rule sessions follow).
	if proj.OwnerID != ownerID {
		return nil, ErrInvalidBinding{Reason: "project not found: " + projectID}
	}
	if proj.SandboxID != sandboxID {
		return nil, ErrInvalidBinding{Reason: "project " + proj.Name + " lives on a different sandbox"}
	}
	return proj, nil
}

// bindingPlan is one run request's resolved sandbox context: the effective
// values the run executes under, and whether this run still owes the session
// its permanent binding (first sandbox-carrying run on an unbound session).
type bindingPlan struct {
	sandboxID string
	projectID string
	needBind  bool
	// revision is the config revision the project was validated against; the
	// bind CAS matches it, so a config updated between plan and write makes
	// the bind lose and re-plan rather than land a stale binding.
	revision int64
}

// planSandboxBinding decides a run's sandbox context WITHOUT writing anything.
// A bound session overrides the request; the client's values are ignored. An
// unbound session carrying a sandbox has the request validated (config must
// exist, project resolved — resolveBindingProject) and a bind planned. Runs
// with no sandbox resolve to none; the session stays bindable. The write
// happens in reserveRun only after hub registration succeeds.
func (r *Runner) planSandboxBinding(ctx context.Context, sess *store.Session, sandboxID, projectID string) (bindingPlan, error) {
	if sess.SandboxID != "" {
		return bindingPlan{sandboxID: sess.SandboxID, projectID: sess.ProjectID}, nil
	}
	if sandboxID == "" {
		return bindingPlan{}, nil
	}
	cfg, err := r.Deps.SandboxConfigs.Get(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bindingPlan{}, ErrInvalidBinding{Reason: "sandbox not found: " + sandboxID}
		}
		return bindingPlan{}, err
	}
	proj, err := r.resolveBindingProject(ctx, sess.OwnerID, sandboxID, projectID)
	if err != nil {
		return bindingPlan{}, err
	}
	return bindingPlan{sandboxID: sandboxID, projectID: proj.ID, needBind: true, revision: cfg.Revision}, nil
}

// BindSessionSandbox binds a still-unbound session to (sandbox, project)
// with no run of its own — for a start that is not a message but carries the
// composer's project: a workflow into a fresh conversation. Same plan and CAS
// as a run's first bind, and the same announcement, broadcast since there is
// no run stream to ride. An empty sandboxID or a session already bound binds
// nothing (false, nil): the standing binding is what the work then uses. A
// CAS lost to a config edit goes around, up to maxBindAttempts, then
// ErrBindingContention — never a start on a session left unbound.
func (r *Runner) BindSessionSandbox(ctx context.Context, sessionID, sandboxID, projectID string) (bool, error) {
	for attempt := 1; ; attempt++ {
		sess, err := r.Deps.Sessions.Get(ctx, sessionID)
		if err != nil {
			return false, err
		}
		plan, err := r.planSandboxBinding(ctx, sess, sandboxID, projectID)
		if err != nil {
			return false, err
		}
		if !plan.needBind {
			return false, nil
		}
		won, err := r.Deps.Sessions.BindSandboxIfEmpty(ctx, sessionID, plan.sandboxID, plan.projectID, plan.revision)
		if err != nil {
			return false, err
		}
		if won {
			if r.OnBroadcast != nil {
				if env, eerr := protocol.NewEnvelope(protocol.EventSessionSandboxBound, protocol.SessionSandboxBound{
					SessionID: sessionID, SandboxID: plan.sandboxID, ProjectID: plan.projectID,
				}); eerr == nil {
					r.OnBroadcast(env, "", sessionID)
				}
			}
			return true, nil
		}
		// Lost: either a run bound the session meanwhile (the next pass sees it
		// and binds nothing) or the config's revision moved.
		if attempt >= maxBindAttempts {
			return false, ErrBindingContention
		}
	}
}

// maxBindAttempts bounds the plan→register→bind loop in reserveRun:
// three passes distinguish an unlucky race from a config under active edit.
const maxBindAttempts = 3

// bindSessionAgent back-fills the session's bound agent config once the run has
// produced an answer. Detached from the run's context: the run is over, and a
// client that hung up must not decide whether the binding lands.
func (r *Runner) bindSessionAgent(sessionID, agentConfigID string) {
	if err := r.Deps.Sessions.BindAgentIfEmpty(context.Background(), sessionID, agentConfigID); err != nil {
		// Best-effort back-fill of the session's bound agent; log rather than
		// swallow so a persistent failure is diagnosable.
		logging.Ctx(r.hub.rootCtx).Warn("updating session agent config", "error", err, "session_id", sessionID)
	}
}

// reserveRun takes the session for a run: plan → register → bind, as ONE
// reservation. Registration (the gate that can refuse) comes before the bind,
// so a run that never starts binds nothing; a first-run bind that loses its
// CAS withdraws the registration and goes around, up to maxBindAttempts. On
// return the slot is held; boundNow reports that THIS run bound the session.
func (r *Runner) reserveRun(runID, sessionID, agentConfigID, sandboxID, projectID string) (seg *runSegment, ctx context.Context, plan bindingPlan, boundNow bool, err error) {
	for attempt := 1; ; attempt++ {
		// Reject unknown sessions up front so we never register a run (or
		// write orphaned messages) against a non-existent session. The same
		// lookup feeds the sandbox binding below.
		sess, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		plan, err = r.planSandboxBinding(r.hub.rootCtx, sess, sandboxID, projectID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		seg, ctx, err = r.hub.register(runID, sessionID, sess.OwnerID, agentConfigID, plan.sandboxID, plan.projectID, meta)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		if !plan.needBind {
			return seg, ctx, plan, false, nil
		}
		won, err := r.Deps.Sessions.BindSandboxIfEmpty(r.hub.rootCtx, sessionID, plan.sandboxID, plan.projectID, plan.revision)
		if err != nil {
			r.hub.unregister(runID, seg)
			return nil, nil, bindingPlan{}, false, err
		}
		if won {
			return seg, ctx, plan, true, nil
		}
		// The CAS refused: the sandbox config was deleted or bumped to a new
		// revision, or the session row was removed. Withdraw the registration and
		// go around; the next pass re-validates, refusing a vanished config (400)
		// or session (404). Only a revision moving every pass keeps the loop
		// alive, and after maxBindAttempts the retry belongs to the client.
		r.hub.unregister(runID, seg)
		if attempt == maxBindAttempts {
			return nil, nil, bindingPlan{}, false, ErrBindingContention
		}
	}
}
