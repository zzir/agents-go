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
// repeatedly. Transient by construction — another run of the same session is
// binding it — so the client retries once it settles. Handlers map it to 409.
var ErrBindingContention = errors.New("the session is being bound; try again")

// ErrInvalidBinding refuses a first-run project binding whose value could
// never work: an unknown project, or one that is not the session owner's.
// Refused at bind time, before anything is written — the binding is
// permanent, so a bad value accepted here would disable the session's sandbox
// for good. Handlers map it to 400.
type ErrInvalidBinding struct{ Reason string }

func (e ErrInvalidBinding) Error() string { return "invalid project binding: " + e.Reason }

// bindingPlan is one run request's resolved sandbox context: the project the
// run executes under, and whether this run still owes the session its
// permanent binding (first project-carrying run on an unbound session).
type bindingPlan struct {
	projectID string
	needBind  bool
}

// planProjectBinding decides a run's sandbox context WITHOUT writing anything.
// A bound session overrides the request; the client's values are ignored. An
// unbound session carrying a project has it validated (it must exist and
// belong to the session's owner) and a bind planned. A run naming no project
// resolves to none — and an agent with no project gets no sandbox tools at
// all (decisions §5.33); the session stays bindable. The write happens in
// reserveRun only after hub registration succeeds.
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
	// A foreign project reads as absent — ownership is not an oracle for
	// existence (the authz rule sessions follow).
	if proj.OwnerID != sess.OwnerID {
		return bindingPlan{}, ErrInvalidBinding{Reason: "project not found: " + projectID}
	}
	return bindingPlan{projectID: proj.ID, needBind: true}, nil
}

// BindSessionProject binds a still-unbound session to a project with no run of
// its own — for a start that is not a message but carries the composer's
// project: a workflow into a fresh conversation. Same plan and CAS as a run's
// first bind, and the same announcement, broadcast since there is no run
// stream to ride. An empty projectID or a session already bound binds nothing
// (false, nil): the standing binding is what the work then uses. A CAS lost
// goes around, up to maxBindAttempts, then ErrBindingContention — never a
// start on a session left unbound.
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
					r.OnBroadcast(env, "", sessionID)
				}
			}
			return true, nil
		}
		// Lost: either a run bound the session meanwhile (the next pass sees it
		// and binds nothing) or the project vanished.
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
func (r *Runner) reserveRun(runID, sessionID, agentConfigID, projectID string) (seg *runSegment, ctx context.Context, plan bindingPlan, boundNow bool, err error) {
	for attempt := 1; ; attempt++ {
		// Reject unknown sessions up front so we never register a run (or
		// write orphaned messages) against a non-existent session. The same
		// lookup feeds the sandbox binding below.
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
			r.hub.unregister(runID, seg)
			return nil, nil, bindingPlan{}, false, err
		}
		if won {
			return seg, ctx, plan, true, nil
		}
		// The CAS refused: another run bound the session first, the project was
		// deleted, or the session row was removed. Withdraw the registration and
		// go around; the next pass re-validates, refusing a vanished project
		// (400) or session (404), or adopting the standing binding. After
		// maxBindAttempts the retry belongs to the client.
		r.hub.unregister(runID, seg)
		if attempt == maxBindAttempts {
			return nil, nil, bindingPlan{}, false, ErrBindingContention
		}
	}
}
