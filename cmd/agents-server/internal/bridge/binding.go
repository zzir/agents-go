package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

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
		if clean == DockerWorkspace {
			return "", nil
		}
		if !dc.Persistent {
			return "", ErrInvalidBinding{Reason: "an ephemeral docker sandbox always runs in /workspace; leave the directory empty"}
		}
		if !strings.HasPrefix(clean, DockerWorkspace+"/") {
			return "", ErrInvalidBinding{Reason: fmt.Sprintf("docker working directory %q must be %s or a subdirectory of it", workDir, DockerWorkspace)}
		}
		return clean, nil
	default:
		// An unknown type fails at buildSandbox with its own error; nothing to
		// validate here.
		return workDir, nil
	}
}
