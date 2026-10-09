package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrBindingContention reports a first-run bind that lost its race
// repeatedly (another run of the session is binding it). Handlers map it to 409.
var ErrBindingContention = errors.New("the session is being bound; try again")

// ErrInvalidBinding refuses, before anything is written, a first-run project
// binding that could never work: an unknown project, or one that is not the
// session owner's (the binding is permanent, invariant 27). Handlers map it to 400.
type ErrInvalidBinding struct{ Reason string }

func (e ErrInvalidBinding) Error() string { return "invalid project binding: " + e.Reason }

// bindingPlan is one run request's resolved sandbox context: the project, and
// whether this run still owes the session its permanent binding.
type bindingPlan struct {
	projectID string
	needBind  bool
}

// planProjectBinding decides a run's sandbox context WITHOUT writing: a bound
// session overrides the request; an unbound one is validated — invariant 27.
func (r *Runner) planProjectBinding(ctx context.Context, sess *store.Session, projectID string) (bindingPlan, error) {
	if sess.ProjectID != "" {
		return bindingPlan{projectID: sess.ProjectID}, nil
	}
	if projectID == "" {
		return bindingPlan{}, nil
	}
	if r.Deps.Projects == nil {
		return bindingPlan{}, fmt.Errorf("no project store is wired")
	}
	proj, err := r.Deps.Projects.Get(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bindingPlan{}, ErrInvalidBinding{Reason: "project not found: " + projectID}
		}
		return bindingPlan{}, err
	}
	// A foreign project reads as absent (ownership is not an existence oracle).
	if proj.OwnerID != sess.OwnerID {
		return bindingPlan{}, ErrInvalidBinding{Reason: "project not found: " + projectID}
	}
	return bindingPlan{projectID: proj.ID, needBind: true}, nil
}

// BindSessionProject binds a still-unbound session to a project with no run
// of its own (a workflow into a fresh conversation), with a run's plan, CAS
// and announcement. An empty projectID or a bound session binds nothing
// (false, nil); a CAS lost retries up to maxBindAttempts, then ErrBindingContention.
func (r *Runner) BindSessionProject(ctx context.Context, sessionID, projectID string) (bool, error) {
	for attempt := 1; ; attempt++ {
		sess, err := r.Deps.Sessions.Get(ctx, sessionID)
		if err != nil {
			return false, err
		}
		plan, err := r.planProjectBinding(ctx, sess, projectID)
		if err != nil {
			return false, err
		}
		if !plan.needBind {
			return false, nil
		}
		won, err := r.Deps.Sessions.BindProjectIfEmpty(ctx, sessionID, plan.projectID)
		if err != nil {
			return false, err
		}
		if won {
			if r.OnBroadcast != nil {
				if env, eerr := protocol.NewEnvelope(protocol.EventSessionProjectBound, protocol.SessionProjectBound{
					SessionID: sessionID, ProjectID: plan.projectID,
				}); eerr == nil {
					r.OnBroadcast(ctx, env, "", sessionID)
				}
			}
			return true, nil
		}
		// Lost: a run bound the session meanwhile, or the project vanished.
		if attempt >= maxBindAttempts {
			return false, ErrBindingContention
		}
	}
}

// maxBindAttempts bounds the plan→register→bind loop in reserveRun.
const maxBindAttempts = 3

// bindSessionAgent back-fills the session's bound agent once the run
// answered, detached from the run's context.
func (r *Runner) bindSessionAgent(sessionID, agentConfigID string) {
	if err := r.Deps.Sessions.BindAgentIfEmpty(context.Background(), sessionID, agentConfigID); err != nil {
		// Best effort, logged.
		logging.Ctx(r.hub.rootCtx).Warn("updating session agent config", "error", err, "session_id", sessionID)
	}
}

// reserveRun takes the session for a run: plan → register → bind as ONE
// reservation (invariant 27). boundNow reports THIS run bound the session.
func (r *Runner) reserveRun(runID, sessionID, agentConfigID, projectID string) (seg *runSegment, ctx context.Context, plan bindingPlan, boundNow bool, err error) {
	for attempt := 1; ; attempt++ {
		// Unknown sessions are rejected up front; the lookup feeds the binding below.
		sess, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		plan, err = r.planProjectBinding(r.hub.rootCtx, sess, projectID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		seg, ctx, err = r.hub.register(runID, sessionID, sess.OwnerID, agentConfigID, plan.projectID, meta)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		if !plan.needBind {
			return seg, ctx, plan, false, nil
		}
		won, err := r.Deps.Sessions.BindProjectIfEmpty(r.hub.rootCtx, sessionID, plan.projectID)
		if err != nil {
			r.withdrawRun(runID, sessionID, seg)
			return nil, nil, bindingPlan{}, false, err
		}
		if won {
			return seg, ctx, plan, true, nil
		}
		// The CAS refused (another run bound it, or the project/session
		// vanished): withdraw and go around.
		r.withdrawRun(runID, sessionID, seg)
		if attempt == maxBindAttempts {
			return nil, nil, bindingPlan{}, false, ErrBindingContention
		}
	}
}
