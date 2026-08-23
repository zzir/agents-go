package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrBindingContention reports a first-run bind that lost its validation race
// repeatedly: every attempt re-validated the working directory against the
// sandbox config's then-current revision, and the config had moved again by
// the time the bind's CAS landed. Transient by construction — the config is
// being actively edited; the client retries once it settles. Handlers map it
// to 409.
var ErrBindingContention = errors.New("sandbox configuration keeps changing; try again")

// ErrInvalidBinding refuses a first-run sandbox binding whose values could
// never work: an unknown sandbox id, or a working directory the sandbox's
// backend cannot honor. Refused at bind time, before anything is written —
// the binding is permanent, so a bad value accepted here would disable the
// session's sandbox for good. Handlers map it to 400.
type ErrInvalidBinding struct{ Reason string }

func (e ErrInvalidBinding) Error() string { return "invalid sandbox binding: " + e.Reason }

// ResolveBindingWorkDir validates the working directory a session is about to
// be permanently bound to and returns its canonical form — the single place
// binding-time workdir rules live. Equivalent spellings must canonicalize to
// one value here, or they key different sandbox instances in the manager's
// cache. Rules per backend:
//
//   - local: empty (the server workspace) or an absolute path.
//   - ssh: a fixed ABSOLUTE directory must exist — from the binding or the
//     config's default. Without one, every exec runs in a fresh remote temp
//     dir and the file tools refuse outright, which breaks the promise a
//     binding makes: one conversation, one file system. Relative is refused
//     even though the ssh user is frozen with the config (identity): a
//     relative binding resolves against a login home the remote host can
//     move, and the stored value would not say where the files are.
//   - docker persistent: empty, /workspace, or a subdirectory of /workspace
//     (the mount point never moves; a session may work in a subtree of it).
//   - docker ephemeral: empty only — each exec is a throw-away container that
//     always runs in /workspace.
//
// effectiveWorkDir stays lenient at RUN time for the same cases: bindings
// written before these rules existed must keep running, not trip a validation
// they never saw.
func ResolveBindingWorkDir(cfg *store.SandboxConfig, workDir string) (string, error) {
	workDir = strings.TrimSpace(workDir)
	switch cfg.Type {
	case "local":
		if workDir == "" {
			return "", nil
		}
		if !filepath.IsAbs(workDir) {
			return "", ErrInvalidBinding{Reason: fmt.Sprintf("local working directory %q must be an absolute path", workDir)}
		}
		return filepath.Clean(workDir), nil
	case "ssh":
		var sc store.SSHConfig
		if len(cfg.Config) > 0 {
			// A stored config that does not decode (predating save-time
			// normalization) must refuse the bind, not bind its zero value:
			// the binding is permanent, and a session bound under a misread
			// config points at the wrong file system for good.
			if err := json.Unmarshal(cfg.Config, &sc); err != nil {
				return "", ErrInvalidBinding{Reason: "this sandbox's stored config cannot be decoded; fix the sandbox first: " + err.Error()}
			}
		}
		if workDir == "" {
			def := strings.TrimSpace(sc.WorkDir)
			if def == "" {
				return "", ErrInvalidBinding{Reason: "this ssh sandbox has no default directory; choose a project directory so the session's files persist between commands"}
			}
			if !path.IsAbs(def) {
				return "", ErrInvalidBinding{Reason: fmt.Sprintf("this ssh sandbox's default directory %q is relative (it resolves against the login home); bind an absolute project directory instead", def)}
			}
			return "", nil
		}
		if !path.IsAbs(workDir) {
			return "", ErrInvalidBinding{Reason: fmt.Sprintf("ssh working directory %q must be an absolute remote path", workDir)}
		}
		return path.Clean(workDir), nil
	case "docker":
		var dc store.DockerConfig
		if len(cfg.Config) > 0 {
			// Same as ssh above: a persistent flag misread as false would
			// validate this bind against the wrong mode.
			if err := json.Unmarshal(cfg.Config, &dc); err != nil {
				return "", ErrInvalidBinding{Reason: "this sandbox's stored config cannot be decoded; fix the sandbox first: " + err.Error()}
			}
		}
		if workDir == "" {
			return "", nil
		}
		// /workspace IS the execution directory in both docker modes — a
		// client that echoes the advertised default back must not be refused
		// over the spelling of "the default".
		clean := path.Clean(workDir)
		if clean == sandboxes.DockerWorkspace {
			return "", nil
		}
		if !dc.Persistent {
			return "", ErrInvalidBinding{Reason: "an ephemeral docker sandbox always runs in /workspace; leave the directory empty"}
		}
		if !strings.HasPrefix(clean, sandboxes.DockerWorkspace+"/") {
			return "", ErrInvalidBinding{Reason: fmt.Sprintf("docker working directory %q must be %s or a subdirectory of it", workDir, sandboxes.DockerWorkspace)}
		}
		return clean, nil
	default:
		// An unknown type fails at buildSandbox with its own error; nothing to
		// validate here.
		return workDir, nil
	}
}

// bindingPlan is one run request's resolved sandbox context: the effective
// values the run executes under, and whether this run still owes the session
// its permanent binding (first sandbox-carrying run on an unbound session).
type bindingPlan struct {
	sandboxID string
	workDir   string
	needBind  bool
	// revision is the config revision the workdir was validated against; the
	// bind CAS matches it, so a config updated between plan and write makes the
	// bind lose and re-plan rather than land a stale workdir.
	revision int64
}

// planSandboxBinding decides a run's sandbox context WITHOUT writing anything.
// A bound session overrides the request; the client's values are ignored. An
// unbound session carrying a sandbox has the request validated (config must
// exist, workdir honored by its backend — ResolveBindingWorkDir) and a bind
// planned. Runs with no sandbox resolve to none; the session stays bindable.
// The write happens in reserveRun only after hub registration succeeds.
func (r *Runner) planSandboxBinding(ctx context.Context, sess *store.Session, sandboxID, workDir string) (bindingPlan, error) {
	if sess.SandboxID != "" {
		return bindingPlan{sandboxID: sess.SandboxID, workDir: sess.WorkDir}, nil
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
	canonical, err := ResolveBindingWorkDir(cfg, workDir)
	if err != nil {
		return bindingPlan{}, err
	}
	return bindingPlan{sandboxID: sandboxID, workDir: canonical, needBind: true, revision: cfg.Revision}, nil
}

// BindSessionSandbox binds a still-unbound session to (sandboxID, workDir)
// with no run of its own — for a start that is not a message but carries the
// composer's project: a workflow into a fresh conversation. Same plan and CAS
// as a run's first bind, and the same announcement, broadcast since there is
// no run stream to ride. An empty sandboxID or a session already bound binds
// nothing (false, nil): the standing binding is what the work then uses. A
// CAS lost to a config edit goes around, up to maxBindAttempts, then
// ErrBindingContention — never a start on a session left unbound.
func (r *Runner) BindSessionSandbox(ctx context.Context, sessionID, sandboxID, workDir string) (bool, error) {
	for attempt := 1; ; attempt++ {
		sess, err := r.Deps.Sessions.Get(ctx, sessionID)
		if err != nil {
			return false, err
		}
		plan, err := r.planSandboxBinding(ctx, sess, sandboxID, workDir)
		if err != nil {
			return false, err
		}
		if !plan.needBind {
			return false, nil
		}
		won, err := r.Deps.Sessions.BindSandboxIfEmpty(ctx, sessionID, plan.sandboxID, plan.workDir, plan.revision)
		if err != nil {
			return false, err
		}
		if won {
			if r.OnBroadcast != nil {
				if env, eerr := protocol.NewEnvelope(protocol.EventSessionSandboxBound, protocol.SessionSandboxBound{
					SessionID: sessionID, SandboxID: plan.sandboxID, WorkDir: plan.workDir,
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
func (r *Runner) reserveRun(runID, sessionID, agentConfigID, sandboxID, workDir string) (seg *runSegment, ctx context.Context, plan bindingPlan, boundNow bool, err error) {
	for attempt := 1; ; attempt++ {
		// Reject unknown sessions up front so we never register a run (or
		// write orphaned messages) against a non-existent session. The same
		// lookup feeds the sandbox binding below.
		sess, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		plan, err = r.planSandboxBinding(r.hub.rootCtx, sess, sandboxID, workDir)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		seg, ctx, err = r.hub.register(runID, sessionID, sess.OwnerID, agentConfigID, plan.sandboxID, plan.workDir, meta)
		if err != nil {
			return nil, nil, bindingPlan{}, false, err
		}
		if !plan.needBind {
			return seg, ctx, plan, false, nil
		}
		won, err := r.Deps.Sessions.BindSandboxIfEmpty(r.hub.rootCtx, sessionID, plan.sandboxID, plan.workDir, plan.revision)
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
