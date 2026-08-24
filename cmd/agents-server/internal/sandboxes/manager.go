// Package sandboxes keeps the live sandbox instances behind stored sandbox
// configs — built on demand, shared across a session, retired on edit — and
// the per-session command trust that gates exec_command.
package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// sandboxKey identifies one live sandbox instance: config id, the RUNTIME
// GENERATION it was built from (content changes only — a rename never splits
// the cache), and the PROJECT whose tree the container mounts. Different
// projects must not share an instance (the mount is baked in at build) —
// see the retired fence for the generation's role.
type sandboxKey struct {
	id        string
	gen       int64
	projectID string
}

// sandboxInstance is one live sandbox plus everything scoped to its lifetime:
// how many holders still use it (runs and terminals, via Acquire) and whether
// an eviction is waiting for the last holder.
type sandboxInstance struct {
	// key is where the instance lives in the cache — the idle timer needs it
	// to evict exactly itself.
	key sandboxKey
	// idle, when set, fires the idle-stop: armed by the release that dropped
	// the last reference, disarmed by the next acquire. Guarded by mu.
	idle *time.Timer
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
	// expired marks an idle expiry mid-stop: the instance stays under its key
	// so an acquire waits on gone instead of adopting a stopping container
	// (which would look dead and be force-recreated, wiping its packages —
	// spec §5.28). gone closes when the stop finished and the key is free.
	// Guarded by mu.
	expired bool
	gone    chan struct{}

	closeOnce sync.Once
}

// close tears down the sandbox, once — the idle expiry and an eviction may
// both reach it. Called without the manager lock — teardown can block on I/O.
// The nil check covers a placeholder whose build never finished (process
// shutdown mid-dial).
func (i *sandboxInstance) close() {
	i.closeOnce.Do(func() {
		if i.sb != nil {
			_ = i.sb.Close()
		}
	})
}

// Manager caches and reuses sandbox instances keyed by (config id,
// runtime generation, project), with a reference count per instance: runs and
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
	buildOverride func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error)
	workspace     string
	// trust holds per-session exec_command approval grants, consulted by the
	// commandGate and updated by the approval resolver.
	trust *TrustStore
	// idleAfter, when set, returns how long an unreferenced instance lives
	// before the idle-stop evicts it (0 = never). Read per release so a
	// settings change applies without a restart. The eviction closes the
	// instance; KeepOnClose makes that a container STOP, and the next
	// acquire re-adopts it (spec §5.28).
	idleAfter func() time.Duration
}

// SetIdleTimeout installs the idle-stop duration provider (see idleAfter).
func (m *Manager) SetIdleTimeout(fn func() time.Duration) { m.idleAfter = fn }

// NewManager creates a Manager that roots local-daemon project trees at workspace.
func NewManager(workspace string) *Manager {
	return &Manager{
		instances: make(map[sandboxKey]*sandboxInstance),
		retired:   make(map[string]int64),
		workspace: workspace,
		trust:     NewTrustStore(),
	}
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

// Acquire returns the cached sandbox for (config, project), building one if
// absent, and takes a reference on it. The returned release MUST be called
// exactly once when the holder is done — a run's teardown, a terminal's
// close. It is idempotent (extra calls are no-ops) and performs the deferred
// close when this holder was the last one keeping a doomed instance alive.
// proj is the working tree the instance's container mounts; every acquire
// carries one.
func (m *Manager) Acquire(cfg *store.SandboxConfig, proj *store.Project) (sandbox.Sandbox, func(), error) {
	inst, release, err := m.acquire(cfg, proj)
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
func (m *Manager) acquire(cfg *store.SandboxConfig, proj *store.Project) (*sandboxInstance, func(), error) {
	if proj == nil {
		return nil, nil, fmt.Errorf("sandbox acquire needs a project")
	}
	key := sandboxKey{id: cfg.ID, gen: cfg.RuntimeGen, projectID: proj.ID}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("sandbox manager is shut down")
	}
	inst, ok := m.instances[key]
	if ok && inst.expired {
		// An idle expiry is stopping this instance's container; wait it out
		// and take the key fresh (the expiry frees it before closing gone).
		gone := inst.gone
		m.mu.Unlock()
		<-gone
		return m.acquire(cfg, proj)
	}
	if !ok {
		inst = &sandboxInstance{ready: make(chan struct{}), key: key}
		inst.refs++
		m.instances[key] = inst
		m.mu.Unlock()

		sb, err := m.buildFn()(cfg, proj)

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
	if inst.idle != nil {
		inst.idle.Stop()
		inst.idle = nil
	}
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
func (m *Manager) buildFn() func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
	if m.buildOverride != nil {
		return m.buildOverride
	}
	return m.buildSandbox
}

// SetBuildOverride replaces the sandbox constructor — tests (in this package
// and the bridge's) inject an in-process fake so tool-execution paths run
// without a Docker daemon.
func (m *Manager) SetBuildOverride(fn func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error)) {
	m.buildOverride = fn
}

// release drops one holder's reference and closes the instance if it was the
// last holder of a doomed one.
func (m *Manager) release(inst *sandboxInstance) {
	// The idle window is a settings read (uncached DB I/O) — resolve it
	// before taking the lock that serializes every acquire and eviction.
	var idle time.Duration
	if m.idleAfter != nil {
		idle = m.idleAfter()
	}
	m.mu.Lock()
	inst.refs--
	dead := inst.doomed && inst.refs <= 0
	if !dead && inst.refs <= 0 && !m.closed && idle > 0 {
		if inst.idle != nil {
			inst.idle.Stop()
		}
		inst.idle = time.AfterFunc(idle, func() { m.idleExpire(inst) })
	}
	m.mu.Unlock()
	if dead {
		inst.close()
	}
}

// idleExpire is the idle timer's body: evict and close the instance — unless
// a new holder arrived (the acquire disarms the timer, but a fire already in
// flight loses this race and must check) or a successor replaced it under
// the key.
func (m *Manager) idleExpire(inst *sandboxInstance) {
	m.mu.Lock()
	if m.instances[inst.key] != inst || inst.refs > 0 {
		m.mu.Unlock()
		return
	}
	// Stop the container BEFORE freeing the key: an acquire during the stop
	// waits on gone rather than adopting a container it would then judge
	// dead and recreate from scratch.
	inst.expired = true
	inst.gone = make(chan struct{})
	m.mu.Unlock()
	inst.close()
	m.mu.Lock()
	if m.instances[inst.key] == inst {
		delete(m.instances, inst.key)
	}
	close(inst.gone)
	m.mu.Unlock()
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

// RemoveInstance evicts the one cached instance serving (config, project) —
// the session-scoped release: when the last session bound to that pair is
// deleted, its container has no caller left, and only a process restart
// would otherwise reclaim the instance. Holders still using it (a run
// mid-flight, an open terminal) keep it alive until their release; the
// eviction only guarantees no NEW holder joins. Other projects on the same
// config keep their instances.
func (m *Manager) RemoveInstance(cfg *store.SandboxConfig, projectID string) {
	key := sandboxKey{id: cfg.ID, gen: cfg.RuntimeGen, projectID: projectID}
	m.mu.Lock()
	inst := m.evictLocked(key)
	m.mu.Unlock()
	if inst != nil {
		inst.close()
	}
}

// RemoveProject evicts every cached instance keyed to the project — its row
// was deleted, so no cached container should idle on for it. The container
// and its storage stay on the daemon (spec §5.28: data outlives the row);
// in-flight holders finish on what they hold.
func (m *Manager) RemoveProject(projectID string) {
	var toClose []*sandboxInstance
	m.mu.Lock()
	for key := range m.instances {
		if key.projectID != projectID {
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
// project variants — and fences the id permanently: the config was deleted,
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

// SandboxTools returns exec_command plus read_file, write_file, list_files and
// apply_patch tools for the given sandbox config, holding a reference on the
// backing instance that the returned release drops (see Acquire — the caller
// releases when the run using the tools is over). apply_patch (Codex-style
// multi-file edits) and the file tools all edit through the same Sandbox, so
// they target the same filesystem exec_command runs in. Every container is
// persistent, so exec_command always offers named shells (session_id); they
// are scoped to this toolset, so the release also closes any the run opened.
// When commandApproval is set, exec_command is gated per call through the
// session command-trust store: a command is approved on first use, then
// trusted per the user's choice.
func (m *Manager) SandboxTools(cfg *store.SandboxConfig, proj *store.Project, commandApproval bool) ([]*agents.Tool, func(), error) {
	sb, release, err := m.Acquire(cfg, proj)
	if err != nil {
		return nil, nil, err
	}
	codeCfg := sandbox.CodeToolConfig{MaxOutputBytes: execToolMaxOutputBytes}
	var pools []io.Closer
	codeCfg.Sessions = true
	codeCfg.RegisterCloser = func(c io.Closer) { pools = append(pools, c) }
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

// buildSandbox constructs the SDK sandbox for (config, project): one
// persistent container per pair, name derived from the two ids, mounting the
// project's tree — a host directory under <workspace>/<user>/<project> on the
// local daemon, the named volume agents-proj-<project> on a remote one (spec
// §5.28). Docker is the only backend (spec §5.27).
func (m *Manager) buildSandbox(cfg *store.SandboxConfig, proj *store.Project) (sandbox.Sandbox, error) {
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
		Persistent:    true,
		KeepOnClose:   true,
		TmpfsSize:     "1g",
		ContainerName: ContainerName(cfg.ID, proj.ID),
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
	if dc.Host == "" {
		opts.WorkDir = m.ProjectHostDir(proj)
		// Bind-mounted files should belong to the user running the server,
		// not nobody — unless the config names its own user.
		if dc.User == "" {
			opts.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		}
	} else {
		opts.VolumeName = "agents-proj-" + shortID(proj.ID)
	}
	opts.MaxReadFileBytes = dc.MaxReadFileBytes
	return dockersb.New(opts)
}

// shortID is a uuid's tail 12 hex chars — enough to tell ids apart in names
// docker and filesystems must carry.
func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 12 {
		id = id[len(id)-12:]
	}
	return id
}

// ContainerName derives the docker container name serving (sandbox, project).
// Deterministic, so a restarted server (or an idle-stopped container) is
// re-adopted by fingerprint instead of duplicated.
func ContainerName(sandboxID, projectID string) string {
	return "agents-" + shortID(sandboxID) + "-" + shortID(projectID)
}

// ProjectHostDir is the local-daemon bind source for a project's /workspace:
// <workspace>/<full owner uuid>/<project id> (spec §5.28 — short ids stay
// where docker imposes name limits, not on the filesystem). Created by the
// SDK at container create.
func (m *Manager) ProjectHostDir(proj *store.Project) string {
	return filepath.Join(m.workspace, proj.OwnerID, proj.ID)
}

// unmarshalConfig decodes a SandboxConfig.Config payload, treating empty as a
// zero-value config so the per-type required-field checks produce the error.
func unmarshalConfig(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
