// Package sandboxes keeps the live sandbox instances behind stored sandbox
// configs — built on demand, shared across a session, retired on edit — and
// the per-session command trust that gates exec_command.
package sandboxes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path"
	"strings"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// DockerWorkspace is the container-side mount point every docker sandbox
// executes under — the execution view of a docker workdir is always this tree.
const DockerWorkspace = "/workspace"

// sandboxKey identifies one live sandbox instance: a config id, the RUNTIME
// GENERATION it was built from (store.SandboxConfig.RuntimeGen — bumped by
// content changes only, so a rename never splits the cache or retires a
// container), and the (normalized) working directory. Sessions bound to
// different workdirs on the same config must not share an instance — the
// workdir is baked in at build time — and the generation keeps a run that
// read the config just before a content update from re-installing an
// old-credential instance under the key runs on the new generation hit.
type sandboxKey struct {
	id      string
	gen     int64
	workDir string
}

// sandboxInstance is one live sandbox plus everything scoped to its lifetime:
// how many holders still use it (runs and terminals, via Acquire) and whether
// an eviction is waiting for the last holder.
type sandboxInstance struct {
	// ready closes once the build finished — sb or buildErr is set from then
	// on. An instance enters the cache as a PLACEHOLDER before its sandbox is
	// dialed (see acquire), so concurrent acquirers of one key wait on this
	// gate instead of dialing again, and they wait OUTSIDE the manager lock.
	ready chan struct{}
	// sb and buildErr are written once, before ready closes; reading them
	// after <-ready needs no lock.
	sb       sandbox.Sandbox
	buildErr error
	// refs counts live holders. Guarded by the manager's mu.
	refs int
	// doomed marks an instance evicted from the cache (config update/delete,
	// or the last bound session going away) while holders remain: nothing new
	// can acquire it, and the LAST release closes it — an in-flight run or an
	// open terminal finishes on the configuration it started with instead of
	// having its connection torn out from under it. Guarded by mu.
	doomed bool
}

// close tears down the sandbox. Called without the manager lock — teardown can
// block on I/O. The nil check covers a placeholder whose build never finished
// (process shutdown mid-dial).
func (i *sandboxInstance) close() {
	if i.sb != nil {
		_ = i.sb.Close()
	}
}

// Manager caches and reuses sandbox instances keyed by (config id,
// runtime generation, workdir), with a reference count per instance: runs and
// terminals Acquire and release, and eviction defers to the last holder (see
// sandboxInstance).
type Manager struct {
	mu        sync.Mutex
	instances map[sandboxKey]*sandboxInstance
	// retired maps a config id to the lowest runtime generation still current
	// — the fence Retire moves on every content-changing config update. An
	// acquire that read the config just before the update builds from a
	// retired generation; the fence makes that instance doomed the moment its
	// build lands, so it serves the run that started it and closes, instead
	// of living in the cache as a stale-credential instance until process
	// exit.
	retired map[string]int64
	// closed latches when CloseAll runs (process shutdown): no new acquire
	// may start, and every instance — building placeholders included — is
	// doomed so the last holder's release closes it.
	closed bool
	// buildOverride, when set (tests only), replaces buildSandbox — see
	// buildFn.
	buildOverride func(*store.SandboxConfig, string) (sandbox.Sandbox, error)
	workspace     string
	// trust holds per-session exec_command approval grants, consulted by the
	// commandGate and updated by the approval resolver.
	trust *TrustStore
}

// NewManager creates a Manager that roots local sandboxes at workspace.
func NewManager(workspace string) *Manager {
	return &Manager{
		instances: make(map[sandboxKey]*sandboxInstance),
		retired:   make(map[string]int64),
		workspace: workspace,
		trust:     NewTrustStore(),
	}
}

// effectiveWorkDir normalizes a per-session workdir for one config — the
// single point where the cache key and the build agree. local/ssh use the
// value (trimmed) as the execution directory. Docker reads it as the
// CONTAINER-side working directory: the /workspace mount point never moves,
// but a persistent container's session may work in a subdirectory of it
// (/workspace itself normalizes to "", the default instance); ephemeral
// containers ignore it entirely.
//
// New bindings are validated and canonicalized up front by
// ResolveBindingWorkDir, so run time normally sees only legal values. The
// out-of-tree fallback (a value outside /workspace lands in the default
// instance) stays because a binding legal when written can be wrong by the time
// it runs — a bind validated against one config revision can land beside an
// update to the next — so such runs degrade to the default directory rather than
// being bricked. Docker subtrees return path.Clean so equivalent spellings key
// one cache entry, not one instance each.
func effectiveWorkDir(cfg *store.SandboxConfig, workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if cfg.Type != "docker" {
		return workDir
	}
	var dc store.DockerConfig
	if len(cfg.Config) > 0 {
		_ = json.Unmarshal(cfg.Config, &dc)
	}
	if !dc.Persistent || workDir == "" {
		return ""
	}
	clean := path.Clean(workDir)
	if clean == DockerWorkspace || !strings.HasPrefix(clean, DockerWorkspace+"/") {
		return ""
	}
	return clean
}

// Trust exposes the session command-trust store so the approval resolver can
// record "allow this command" / "allow all" grants for a session.
func (m *Manager) Trust() *TrustStore { return m.trust }

// commandGate is exec_command's per-call approval gate: approval is required
// unless the run's session has already trusted this exact command (or all
// commands). The session id rides in RunContext.Context, set by the runner.
func (m *Manager) commandGate(_ context.Context, rc *agents.RunContext, argsJSON string, _ string) (bool, error) {
	if rc == nil {
		return true, nil
	}
	sid, _ := rc.Context.(string)
	if sid == "" {
		return true, nil // no session context → be safe, require approval
	}
	return !m.trust.ForSession(sid).trusted(CommandHash(argsJSON)), nil
}

// Acquire returns the cached sandbox for (config, workDir), building one if
// absent, and takes a reference on it. The returned release MUST be called
// exactly once when the holder is done — a run's teardown, a terminal's
// close, a health check's end. It is idempotent (extra calls are no-ops) and
// performs the deferred close when this holder was the last one keeping a
// doomed instance alive. workDir "" means the config's own default; callers
// outside a bound session (terminal panel, config test) pass "".
func (m *Manager) Acquire(cfg *store.SandboxConfig, workDir string) (sandbox.Sandbox, func(), error) {
	inst, release, err := m.acquire(cfg, workDir)
	if err != nil {
		return nil, nil, err
	}
	return inst.sb, release, nil
}

// acquire backs Acquire, returning the instance itself.
//
// The build runs OUTSIDE the manager lock. An ssh dial can take seconds (the
// connect timeout defaults to 15s), and holding the lock through it would
// stall every other key — unrelated acquires, evictions, terminal opens —
// behind one unreachable host. The lock covers only the map: a first
// acquirer installs a placeholder and dials after unlocking; concurrent
// acquirers of the SAME key find the placeholder, take their reference, and
// wait on its ready gate — one dial, keyed contention only.
func (m *Manager) acquire(cfg *store.SandboxConfig, workDir string) (*sandboxInstance, func(), error) {
	key := sandboxKey{id: cfg.ID, gen: cfg.RuntimeGen, workDir: effectiveWorkDir(cfg, workDir)}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("sandbox manager is shut down")
	}
	inst, ok := m.instances[key]
	if !ok {
		inst = &sandboxInstance{ready: make(chan struct{})}
		inst.refs++
		m.instances[key] = inst
		m.mu.Unlock()

		sb, err := m.buildFn()(cfg, key.workDir)

		m.mu.Lock()
		inst.sb, inst.buildErr = sb, err
		switch {
		case err != nil:
			// A failed build must not poison the key: the placeholder leaves
			// the cache so the next acquire retries with a fresh dial. Waiters
			// already holding the placeholder read buildErr off it. (The ==
			// guard: an eviction may have removed it — or a successor may
			// occupy the key — while the dial was in flight.)
			if m.instances[key] == inst {
				delete(m.instances, key)
			}
		case m.instances[key] != inst:
			// Evicted while dialing (a config update's Retire, CloseAll, the
			// last bound session going). The evictor saw refs > 0 and marked
			// the placeholder doomed, so the last release closes what was just
			// built — nothing to do here; the case exists to say so.
		case m.retired[cfg.ID] > cfg.RuntimeGen || m.closed:
			// Built from a generation that was retired while the dial was in
			// flight (the acquire read the config just before an update), or
			// the manager shut down meanwhile. Serve the holders that are
			// already waiting — their run validly started on this generation —
			// but out of the cache and doomed: the last release closes it, and
			// no later acquire can share a stale-credential instance.
			delete(m.instances, key)
			inst.doomed = true
		}
		close(inst.ready)
		m.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		var once sync.Once
		return inst, func() { once.Do(func() { m.release(inst) }) }, nil
	}
	inst.refs++
	m.mu.Unlock()

	// Usually a closed gate (any built instance); a placeholder's waiters
	// block here — outside the lock — until its dial settles.
	<-inst.ready
	if inst.buildErr != nil {
		// The abandoned placeholder is out of the cache (the builder removed
		// it) and unreachable; the reference taken above dies with it.
		return nil, nil, inst.buildErr
	}
	var once sync.Once
	release := func() { once.Do(func() { m.release(inst) }) }
	return inst, release, nil
}

// buildFn returns the sandbox builder — the real one, or a test's injected
// stand-in (the only way to hold a build open while exercising the concurrent
// eviction and shutdown paths).
func (m *Manager) buildFn() func(*store.SandboxConfig, string) (sandbox.Sandbox, error) {
	if m.buildOverride != nil {
		return m.buildOverride
	}
	return m.buildSandbox
}

// SetBuildOverride replaces the sandbox constructor — tests (in this package
// and the bridge's) inject an in-process fake so tool-execution paths run
// without a Docker daemon.
func (m *Manager) SetBuildOverride(fn func(*store.SandboxConfig, string) (sandbox.Sandbox, error)) {
	m.buildOverride = fn
}

// release drops one holder's reference and closes the instance if it was the
// last holder of a doomed one.
func (m *Manager) release(inst *sandboxInstance) {
	m.mu.Lock()
	inst.refs--
	dead := inst.doomed && inst.refs <= 0
	m.mu.Unlock()
	if dead {
		inst.close()
	}
}

// evictLocked removes an instance from the cache and reports whether the
// caller should close it now: with holders remaining it is doomed instead,
// and the last release closes it. Callers hold m.mu.
func (m *Manager) evictLocked(key sandboxKey) (toClose *sandboxInstance) {
	inst, ok := m.instances[key]
	if !ok {
		return nil
	}
	delete(m.instances, key)
	if inst.refs > 0 {
		inst.doomed = true
		return nil
	}
	return inst
}

// RemoveInstance evicts the one cached instance serving (config, workDir) —
// the session-scoped release: when the last session bound to that pair is
// deleted, its ssh connection or docker container has no caller left, and
// only a process restart would otherwise reclaim it. Holders still using the
// instance (a run mid-flight, an open terminal) keep it alive until their
// release; the eviction only guarantees no NEW holder joins. Other workdirs
// on the same config keep their instances.
func (m *Manager) RemoveInstance(cfg *store.SandboxConfig, workDir string) {
	key := sandboxKey{id: cfg.ID, gen: cfg.RuntimeGen, workDir: effectiveWorkDir(cfg, workDir)}
	m.mu.Lock()
	inst := m.evictLocked(key)
	m.mu.Unlock()
	if inst != nil {
		inst.close()
	}
}

// Retire evicts every cached instance of the config id built from a runtime
// generation below minLive, and moves the fence so none can come back: an
// acquire that read the config just before the update may still be dialing,
// and without the fence its build would re-install an old-credential
// instance after the eviction swept the cache. With it, that instance is
// doomed the moment its build lands. In-flight holders finish on what they
// acquired; only idle instances close immediately.
func (m *Manager) Retire(id string, minLive int64) {
	var toClose []*sandboxInstance
	m.mu.Lock()
	if m.retired[id] < minLive {
		m.retired[id] = minLive
	}
	for key := range m.instances {
		if key.id != id || key.gen >= minLive {
			continue
		}
		if inst := m.evictLocked(key); inst != nil {
			toClose = append(toClose, inst)
		}
	}
	m.mu.Unlock()
	for _, inst := range toClose {
		inst.close()
	}
}

// Remove evicts every cached instance of the config id — all generations and
// workdir variants — and fences the id permanently: the config was deleted,
// so nothing may serve it again. The tombstone covers callers who READ the
// config before the delete (a terminal open or config test reads, dials,
// then acquires): without it their late build would enter the cache as an
// instance of a config that no longer exists, with no path ever retiring it.
// Permanence is safe — ids are random and never reused. In-flight holders
// finish on what they acquired; only idle instances close immediately.
func (m *Manager) Remove(id string) {
	var toClose []*sandboxInstance
	m.mu.Lock()
	m.retired[id] = math.MaxInt64
	for key := range m.instances {
		if key.id != id {
			continue
		}
		if inst := m.evictLocked(key); inst != nil {
			toClose = append(toClose, inst)
		}
	}
	m.mu.Unlock()
	for _, inst := range toClose {
		inst.close()
	}
}

// CloseAll evicts everything and latches the manager closed — the process is
// exiting. Idle instances close now; held ones (a run in teardown, a terminal
// draining, a build still dialing) are doomed and close on their last
// release, which is also what keeps this free of the builder's own writes: an
// instance is only ever closed after its ready gate, never while the dial is
// in flight.
func (m *Manager) CloseAll() {
	var toClose []*sandboxInstance
	m.mu.Lock()
	m.closed = true
	for key := range m.instances {
		if inst := m.evictLocked(key); inst != nil {
			toClose = append(toClose, inst)
		}
	}
	m.mu.Unlock()
	for _, inst := range toClose {
		inst.close()
	}
}

// Output caps for the sandbox tools, replacing the SDK's 8192 defaults:
// read_file must return whole source files (65536 covers a ~2000-line file),
// while exec_command keeps a tighter per-stream cap — its truncation preserves
// head and tail, which is what build/test output needs.
const (
	fileToolMaxOutputBytes = 65536
	execToolMaxOutputBytes = 32768
)

// TerminalCapable reports whether a sandbox config can hold an interactive
// shell open (sandbox.TerminalOpener): only a persistent container can — an
// ephemeral one has nothing to attach to between Execs. One rule for both
// consumers: the web-terminal capability flag and exec_command's persistent
// sessions.
func TerminalCapable(cfg *store.SandboxConfig) bool {
	var dc store.DockerConfig
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &dc); err != nil {
			return false
		}
	}
	return dc.Persistent
}

// SandboxTools returns exec_command plus read_file, write_file, list_files and
// apply_patch tools for the given sandbox config, holding a reference on the
// backing instance that the returned release drops (see Acquire — the caller
// releases when the run using the tools is over). apply_patch (Codex-style
// multi-file edits) and the file tools all edit through the same Sandbox, so
// they target the same filesystem exec_command runs in. On terminal-capable
// backends exec_command offers persistent named shells (session_id); they are
// scoped to this toolset, so the release also closes any the run opened. When
// commandApproval is set, exec_command is gated per call through the session
// command-trust store: a command is approved on first use, then trusted per
// the user's choice.
func (m *Manager) SandboxTools(cfg *store.SandboxConfig, workDir string, commandApproval bool) ([]*agents.Tool, func(), error) {
	sb, release, err := m.Acquire(cfg, workDir)
	if err != nil {
		return nil, nil, err
	}
	codeCfg := sandbox.CodeToolConfig{MaxOutputBytes: execToolMaxOutputBytes}
	var pools []io.Closer
	if TerminalCapable(cfg) {
		codeCfg.Sessions = true
		codeCfg.RegisterCloser = func(c io.Closer) { pools = append(pools, c) }
	}
	if commandApproval {
		codeCfg.NeedsApprovalFunc = m.commandGate
	}
	fileCfg := sandbox.FileToolConfig{MaxOutputBytes: fileToolMaxOutputBytes}
	tools := []*agents.Tool{sandbox.CodeTool(sb, codeCfg)}
	tools = append(tools, sandbox.FileTools(sb, fileCfg)...)
	tools = append(tools, sandbox.ApplyPatchTool(sb, fileCfg))
	releaseTools := func() {
		for _, p := range pools {
			_ = p.Close()
		}
		release()
	}
	return tools, releaseTools, nil
}

// buildSandbox constructs the SDK sandbox for a config. workDir is the
// session's bound working directory ("" = the config's default view); docker
// is the only backend (spec §5.27).
func (m *Manager) buildSandbox(cfg *store.SandboxConfig, workDir string) (sandbox.Sandbox, error) {
	if cfg.Type != "docker" {
		return nil, fmt.Errorf("unknown sandbox type: %s", cfg.Type)
	}
	var dc store.DockerConfig
	if err := unmarshalConfig(cfg.Config, &dc); err != nil {
		return nil, fmt.Errorf("docker sandbox: invalid config: %w", err)
	}
	if dc.Image == "" {
		return nil, fmt.Errorf("docker sandbox requires an image")
	}
	opts := dockersb.Options{
		Image:   dc.Image,
		Host:    dc.Host,
		Runtime: dc.Runtime,
		User:    dc.User,
		Network: dc.Network,
		Limits: sandbox.Limits{
			MemoryBytes: dc.MemoryMB << 20,
			CPUs:        dc.CPUs,
		},
		Persistent:       dc.Persistent,
		ContainerName:    dc.ContainerName,
		MaxReadFileBytes: dc.MaxReadFileBytes,
	}
	if strings.HasPrefix(dc.Host, "ssh://") {
		opts.SSH = dockersb.SSHAuth{
			UseAgent:              dc.SSHUseAgent,
			KeyFile:               dc.SSHKeyFile,
			Password:              dc.SSHPassword,
			KnownHostsFile:        dc.SSHKnownHosts,
			InsecureIgnoreHostKey: dc.SSHInsecureHostKey,
		}
	}
	if dc.Persistent {
		hostDir := dc.HostDir
		if hostDir == "" {
			hostDir = m.workspace
		}
		// A bind mount is a LOCAL-daemon feature: the path means nothing on a
		// remote host. Remote persistent containers keep their own /workspace
		// volume until projects land.
		if hostDir != "" && dc.Host == "" {
			opts.WorkDir = hostDir
		}
		// The session's project subtree inside the mount ("" = /workspace
		// itself). Only meaningful with a workdir per session; ephemeral
		// containers never get one (effectiveWorkDir).
		opts.ContainerWorkDir = workDir
		// A fixed container name belongs to the default instance; a
		// subtree instance is a SECOND container over the same mount and
		// must not fight it for the name.
		if opts.ContainerName != "" && workDir != "" {
			sum := sha256.Sum256([]byte(workDir))
			opts.ContainerName = fmt.Sprintf("%s-%x", opts.ContainerName, sum[:4])
		}
	}
	return dockersb.New(opts)
}

// unmarshalConfig decodes a SandboxConfig.Config payload, treating empty as a
// zero-value config so the per-type required-field checks produce the error.
func unmarshalConfig(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
