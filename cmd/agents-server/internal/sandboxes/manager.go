// Package sandboxes keeps the live sandbox instances behind stored projects —
// built on demand, shared across a session, retired on edit — and the
// per-session command trust that gates exec_command.
package sandboxes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// Spec is everything one project's sandbox is built from: the sandbox — where
// it runs and what it runs — and the project whose tree it mounts.
type Spec struct {
	Sandbox *store.Sandbox
	Project *store.Project
	// SaveInstanceRef records the handle a service minted for this project's
	// sandbox, so the next process finds it. The MANAGER fills it in; a backend
	// that derives its own name (docker) never calls it.
	SaveInstanceRef func(ctx context.Context, ref string) error
}

// sandboxKey identifies one live instance: the PROJECT and its RUNTIME
// GENERATION — one axis, moved by every content change (decisions §5.33).
type sandboxKey struct {
	projectID string
	gen       int64
}

// sandboxInstance is one live sandbox plus what is scoped to its lifetime:
// its holders (Acquire) and whether an eviction waits for the last one.
type sandboxInstance struct {
	// key is where the instance lives in the cache — the idle timer needs it
	// to evict exactly itself.
	key sandboxKey
	// idle, when set, fires the idle-stop: armed by the release that dropped
	// the last reference, disarmed by the next acquire. Guarded by mu.
	idle *time.Timer
	// ready closes once the build finished (sb or buildErr set). An instance
	// enters the cache as a PLACEHOLDER, so concurrent acquirers wait here, OUTSIDE mu.
	ready chan struct{}
	// sb and buildErr are written once, before ready closes; reading them
	// after <-ready needs no lock.
	sb       sandbox.Sandbox
	buildErr error
	// refs counts live holders. Guarded by the manager's mu.
	refs int
	// doomed marks an instance evicted while holders remain: nothing new
	// acquires it, and the LAST release closes it (invariant 27). Guarded by mu.
	doomed bool
	// expired marks a stop in flight: the instance stays under its key so an
	// acquire waits on gone (decisions §5.28) rather than adopting it. Guarded by mu.
	expired bool
	gone    chan struct{}
	// stopOnRelease PAUSES the compute when the last holder of a doomed instance
	// releases (a deferred Stop, an idle expiry); retire/remove/rebuild leave it false.
	stopOnRelease bool

	closeOnce sync.Once
}

// stopTimeout bounds a background pause/stop — an idle expiry or a deferred
// user Stop, neither of which has the caller's context to carry.
const stopTimeout = 30 * time.Second

// close tears down the sandbox once, without the manager lock (teardown can
// block on I/O); the nil check covers a placeholder whose build never finished.
func (i *sandboxInstance) close() {
	i.closeOnce.Do(func() {
		if i.sb != nil {
			_ = i.sb.Close()
		}
	})
}

// stop pauses the compute (Lifecycle.Stop) before releasing the connection:
// for e2b that pause is the only thing that ends the billed sandbox.
func (i *sandboxInstance) stop() {
	if lc, ok := i.sb.(sandbox.Lifecycle); ok {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		_ = lc.Stop(ctx)
		cancel()
	}
	i.close()
}

// Manager caches and reuses sandbox instances keyed by (project, runtime
// generation), with a reference count per instance: runs and terminals
// Acquire and release, and eviction defers to the last holder (see
// sandboxInstance).
type Manager struct {
	mu        sync.Mutex
	instances map[sandboxKey]*sandboxInstance
	// retired maps a project id to the lowest generation still current — the
	// fence RetireProject moves; an in-flight build from below it lands doomed.
	retired map[string]int64
	// closed latches on CloseAll: no new acquire, and every instance is doomed
	// so the last holder's release closes it.
	closed bool
	// buildOverride, when set (tests only), replaces buildSandbox — see
	// buildFn.
	buildOverride func(Spec) (sandbox.Sandbox, error)
	// trust holds per-session exec_command approval grants, consulted by the
	// commandGate and updated by the approval resolver.
	trust *TrustStore
	// writeInstanceRef persists a backend's handle on a project's sandbox; without
	// it the build refuses rather than provisioning a fresh sandbox per restart.
	writeInstanceRef func(ctx context.Context, projectID, ref string) error
	// idleAfter, when set, is how long an unreferenced instance lives (0 =
	// never), read per release; the eviction STOPS the container (decisions §5.28).
	idleAfter func() time.Duration
}

// SetIdleTimeout installs the idle-stop duration provider (see idleAfter).
func (m *Manager) SetIdleTimeout(fn func() time.Duration) { m.idleAfter = fn }

// SetInstanceRefWriter installs how a backend's handle on a project's sandbox
// is persisted.
func (m *Manager) SetInstanceRefWriter(fn func(ctx context.Context, projectID, ref string) error) {
	m.writeInstanceRef = fn
}

// NewManager creates an empty Manager.
func NewManager() *Manager {
	return &Manager{
		instances: make(map[sandboxKey]*sandboxInstance),
		retired:   make(map[string]int64),
		trust:     NewTrustStore(),
	}
}

// Trust exposes the session command-trust store so the approval resolver can
// record "allow this command" / "allow all" grants for a session.
func (m *Manager) Trust() *TrustStore { return m.trust }

// commandGate is exec_command's per-call approval gate: required unless the
// session trusted this command (or all). The session id rides in RunContext.Context.
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

// Acquire returns the cached sandbox for spec's project, building one if
// absent, and takes a reference on it. The returned release MUST be called
// exactly once when the holder is done — a run's teardown, a terminal's
// close. It is idempotent (extra calls are no-ops) and performs the deferred
// close when this holder was the last one keeping a doomed instance alive.
func (m *Manager) Acquire(spec Spec) (sandbox.Sandbox, func(), error) {
	inst, release, err := m.acquire(spec)
	if err != nil {
		return nil, nil, err
	}
	return inst.sb, release, nil
}

// acquire backs Acquire. The build runs OUTSIDE the manager lock (an ssh dial
// takes seconds): the first acquirer installs a placeholder, the rest wait on ready.
func (m *Manager) acquire(spec Spec) (*sandboxInstance, func(), error) {
	if spec.Project == nil || spec.Sandbox == nil {
		return nil, nil, fmt.Errorf("sandbox acquire needs a sandbox and a project")
	}
	key := sandboxKey{projectID: spec.Project.ID, gen: spec.Project.RuntimeGen}
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
		return m.acquire(spec)
	}
	if !ok {
		inst = &sandboxInstance{ready: make(chan struct{}), key: key}
		inst.refs++
		m.instances[key] = inst
		m.mu.Unlock()

		sb, err := m.buildFn()(spec)

		m.mu.Lock()
		inst.sb, inst.buildErr = sb, err
		switch {
		case err != nil:
			// A failed build must not poison the key: the placeholder leaves the
			// cache (if still ours — an evictor or successor may hold the key).
			if m.instances[key] == inst {
				delete(m.instances, key)
			}
		case m.instances[key] != inst:
			// Evicted while dialing: the evictor marked the placeholder doomed,
			// so the last release closes what was just built.
		case m.retired[key.projectID] > key.gen || m.closed:
			// Built from a generation retired mid-dial, or after shutdown: serve
			// the holders already waiting, out of the cache and doomed.
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

// buildFn returns the sandbox builder — the real one, or a test's stand-in
// (the only way to hold a build open in the concurrency tests).
func (m *Manager) buildFn() func(Spec) (sandbox.Sandbox, error) {
	if m.buildOverride != nil {
		return m.buildOverride
	}
	return m.buildSandbox
}

// SetBuildOverride replaces the sandbox constructor — tests (in this package
// and the bridge's) inject an in-process fake so tool-execution paths run
// without a Docker daemon.
func (m *Manager) SetBuildOverride(fn func(Spec) (sandbox.Sandbox, error)) {
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
	stopIntent := inst.stopOnRelease
	if dead && stopIntent {
		if m.projectCachedLocked(inst.key.projectID) {
			// New work acquired the project after the deferred Stop: the stale
			// stop is superseded; only this connection closes.
			stopIntent = false
		} else {
			// Fence the pause like the idle expiry: reclaim the key as an
			// expired placeholder so a racing Acquire waits the stop out.
			inst.expired = true
			inst.gone = make(chan struct{})
			m.instances[inst.key] = inst
		}
	}
	if !dead && inst.refs <= 0 && !m.closed && idle > 0 {
		if inst.idle != nil {
			inst.idle.Stop()
		}
		inst.idle = time.AfterFunc(idle, func() { m.idleExpire(inst) })
	}
	m.mu.Unlock()
	if dead {
		if stopIntent {
			m.stopFenced(inst)
		} else {
			inst.close()
		}
	}
}

// projectCachedLocked reports whether any instance of the project occupies the
// cache — live, building, or mid-stop. Callers hold m.mu.
func (m *Manager) projectCachedLocked(projectID string) bool {
	for key := range m.instances {
		if key.projectID == projectID {
			return true
		}
	}
	return false
}

// idleExpire is the idle timer's body: evict and stop — unless a holder arrived
// (a fire in flight loses that race), a successor holds the key, or a Stop owns it.
func (m *Manager) idleExpire(inst *sandboxInstance) {
	m.mu.Lock()
	if m.instances[inst.key] != inst || inst.refs > 0 || inst.expired {
		m.mu.Unlock()
		return
	}
	// Fence BEFORE stopping: an acquire during the stop waits on gone rather
	// than adopting a container it would judge dead (decisions §5.28).
	inst.expired = true
	inst.gone = make(chan struct{})
	m.mu.Unlock()
	m.stopFenced(inst)
}

// stopFenced pauses inst's compute, then frees its key and waiters; the caller
// set the expired/gone fence under the lock.
func (m *Manager) stopFenced(inst *sandboxInstance) {
	inst.stop()
	m.mu.Lock()
	if m.instances[inst.key] == inst {
		delete(m.instances, inst.key)
	}
	close(inst.gone)
	m.mu.Unlock()
}

// evictLocked removes an instance from the cache and returns it when the caller
// should close it now; with holders it is doomed instead. Callers hold m.mu.
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

// RetireProject evicts every instance of a project built before minLive and
// fences that generation off, so an in-flight build cannot repopulate the cache
// with the old configuration; live holders finish on what they have (invariant 27).
func (m *Manager) RetireProject(projectID string, minLive int64) {
	var toClose []*sandboxInstance
	m.mu.Lock()
	if m.retired[projectID] < minLive {
		m.retired[projectID] = minLive
	}
	for key := range m.instances {
		if key.projectID != projectID || key.gen >= minLive {
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

// RemoveProject evicts every cached instance of the project and fences its id
// permanently (ids are never reused), so a late build cannot re-enter the cache.
// In-flight holders finish on what they acquired.
func (m *Manager) RemoveProject(projectID string) {
	var toClose []*sandboxInstance
	m.mu.Lock()
	m.retired[projectID] = maxGen
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

// maxGen fences a project id permanently: no runtime generation can reach it.
const maxGen = int64(1) << 62

// ReclaimProject destroys the project's compute AND its storage, after
// evicting the cached instance — deleting a project deletes its files
// (decisions §5.33). The caller deletes the row first, so a failure here
// leaves reclaimable storage rather than a row pointing at nothing.
func (m *Manager) ReclaimProject(ctx context.Context, spec Spec) error {
	m.RemoveProject(spec.Project.ID)
	b, err := backendFor(spec)
	if err != nil {
		return err
	}
	return b.Reclaim(ctx, spec)
}

// RebuildContainer discards the project's compute and provisions it again
// from the current template and environment. What "discard" means is the
// backend's (workbench invariant 44); in-flight commands in the old container
// fail, and the caller warns before taking that deal.
func (m *Manager) RebuildContainer(ctx context.Context, spec Spec) error {
	b, err := backendFor(spec)
	if err != nil {
		return err
	}
	m.EvictProject(spec.Project.ID)
	if err := b.Rebuild(ctx, spec); err != nil {
		return err
	}
	return m.EnsureRunning(ctx, spec)
}

// Check reports whether the sandbox is reachable and runnable. It touches no
// project: a health check must not create one, and must not leave anything
// behind.
func (m *Manager) Check(ctx context.Context, sb *store.Sandbox) error {
	b, err := BackendFor(sb.Type)
	if err != nil {
		return err
	}
	return b.Check(ctx, sb)
}

// EvictProject drops the project's cached instances without fencing the id —
// the eviction a rebuild and a last-session-released binding both need, which
// must leave the project acquirable afterwards.
func (m *Manager) EvictProject(projectID string) {
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

// stopProjectOnRelease evicts every instance of the project and makes the last
// holder's release PAUSE it (a user Stop deferred behind live work); idle ones now.
func (m *Manager) stopProjectOnRelease(projectID string) {
	var toStop []*sandboxInstance
	m.mu.Lock()
	for key, inst := range m.instances {
		if key.projectID != projectID || inst.expired {
			continue // an idle expiry mid-stop already owns that one's teardown
		}
		inst.stopOnRelease = true
		if inst.refs > 0 {
			// Holders keep it; the key frees now so nothing new joins, and the
			// last release pauses it (see release).
			delete(m.instances, key)
			inst.doomed = true
			continue
		}
		// No holders: stop now, fenced under its key like the idle expiry; a
		// timer fire already in flight no-ops on expired.
		if inst.idle != nil {
			inst.idle.Stop()
			inst.idle = nil
		}
		inst.expired = true
		inst.gone = make(chan struct{})
		toStop = append(toStop, inst)
	}
	m.mu.Unlock()
	for _, inst := range toStop {
		m.stopFenced(inst)
	}
}

// CloseAll evicts everything and latches the manager closed. Idle instances
// close now; held ones (including a build still dialing) are doomed and close
// on their last release — always after their ready gate.
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

// Output caps for the sandbox tools, above the SDK's 8192 defaults: read_file
// must return whole source files; exec_command keeps head and tail.
const (
	fileToolMaxOutputBytes = 65536
	execToolMaxOutputBytes = 32768
)

// SandboxTools returns exec_command plus read_file, write_file, list_files and
// apply_patch for the given project, all over the one Sandbox, holding a
// reference the returned release drops (see Acquire). exec_command offers
// named shells (session_id) when the sandbox can hold a PTY open; they are
// scoped to this toolset, so the release closes any the run opened. When
// commandApproval is set, exec_command is gated per call through the session
// command-trust store.
func (m *Manager) SandboxTools(spec Spec, commandApproval bool) ([]*agents.Tool, func(), error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return nil, nil, err
	}
	codeCfg := sandbox.CodeToolConfig{MaxOutputBytes: execToolMaxOutputBytes}
	var pools []io.Closer
	// The schema advertises session_id only when the backend can actually hold
	// a shell open — spec §2.7k's conditional-schema rule.
	_, codeCfg.Sessions = sb.(sandbox.TerminalOpener)
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

// buildSandbox hands spec to its target type's backend, with the callback a
// remote backend needs to remember what it provisioned.
func (m *Manager) buildSandbox(spec Spec) (sandbox.Sandbox, error) {
	b, err := backendFor(spec)
	if err != nil {
		return nil, err
	}
	projectID := spec.Project.ID
	spec.SaveInstanceRef = func(ctx context.Context, ref string) error {
		if m.writeInstanceRef == nil {
			return fmt.Errorf("no way to record the sandbox for project %s: nothing would ever stop it", projectID)
		}
		return m.writeInstanceRef(ctx, projectID, ref)
	}
	return b.Open(spec)
}

// EnsureRunning provisions the project's sandbox and makes it ready to take
// commands, rather than leaving that to the first command. It is what a
// "Start" button and a rebuild both need: an image pull's worth of waiting
// happens here, where a person is watching, instead of inside a run.
func (m *Manager) EnsureRunning(ctx context.Context, spec Spec) error {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return err
	}
	defer release()
	lc, ok := sb.(sandbox.Lifecycle)
	if !ok {
		return fmt.Errorf("%s sandbox: %w", spec.Sandbox.Type, sandbox.ErrLifecycleUnsupported)
	}
	return lc.Start(ctx)
}

// Stop releases the project's compute, keeping its storage. It reports
// whether the sandbox stopped NOW: with another holder — a run in flight, an
// open terminal — the instance is only doomed, and the last release stops it.
// Tearing a live run off its container would be the other option, and it is
// not one: the person asked for the sandbox to stop, not for the work to die.
func (m *Manager) Stop(ctx context.Context, spec Spec) (stopped bool, err error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return false, err
	}
	defer release()
	lc, ok := sb.(sandbox.Lifecycle)
	if !ok {
		return false, fmt.Errorf("%s sandbox: %w", spec.Sandbox.Type, sandbox.ErrLifecycleUnsupported)
	}
	// "Sole holder?" and "fence as stopping" under one lock, or a concurrent
	// Acquire between them builds against the container this call stops.
	finish, sole := m.detachIfSole(spec.Project.ID)
	if !sole {
		// Someone is still working in it: the last holder's release PAUSES it
		// (close alone releases nothing remote for e2b).
		m.stopProjectOnRelease(spec.Project.ID)
		return false, nil
	}
	err = lc.Stop(ctx)
	finish()
	return err == nil, err
}

// detachIfSole reports whether this caller holds the project's only reference and,
// if so, fences every instance as stopping (idleExpire's shape); finish frees the keys.
func (m *Manager) detachIfSole(projectID string) (finish func(), sole bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.projectRefsLocked(projectID) != 1 {
		return nil, false
	}
	var fenced []*sandboxInstance
	for key, inst := range m.instances {
		if key.projectID != projectID || inst.expired {
			continue // an idle expiry mid-stop frees its own key
		}
		inst.doomed = true
		if inst.idle != nil {
			inst.idle.Stop()
			inst.idle = nil
		}
		inst.expired = true
		inst.gone = make(chan struct{})
		fenced = append(fenced, inst)
	}
	return func() {
		m.mu.Lock()
		for _, inst := range fenced {
			if m.instances[inst.key] == inst {
				delete(m.instances, inst.key)
			}
			close(inst.gone)
		}
		m.mu.Unlock()
	}, true
}

// projectRefsLocked sums the live holders across every cached generation of
// the project. Callers hold m.mu.
func (m *Manager) projectRefsLocked(projectID string) int {
	n := 0
	for key, inst := range m.instances {
		if key.projectID == projectID {
			n += inst.refs
		}
	}
	return n
}

// Status reports what the project's compute is doing. It builds the sandbox
// (a connection, not a container), so "never started" answers absent rather
// than failing.
func (m *Manager) Status(ctx context.Context, spec Spec) (sandbox.State, error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return sandbox.StateAbsent, err
	}
	defer release()
	lc, ok := sb.(sandbox.Lifecycle)
	if !ok {
		return sandbox.StateAbsent, fmt.Errorf("%s sandbox: %w", spec.Sandbox.Type, sandbox.ErrLifecycleUnsupported)
	}
	return lc.Status(ctx)
}

// ExportProject streams the project's working tree as a tar archive. The
// reference the export holds is released when the returned reader is closed:
// the archive is produced lazily by the backend, so releasing at return would
// let an eviction close the connection mid-stream.
func (m *Manager) ExportProject(ctx context.Context, spec Spec) (io.ReadCloser, error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return nil, err
	}
	ex, ok := sb.(sandbox.Exporter)
	if !ok {
		release()
		return nil, fmt.Errorf("%s sandbox: cannot export", spec.Sandbox.Type)
	}
	rc, err := ex.ExportTar(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &releasingReader{ReadCloser: rc, release: release}, nil
}

// releasingReader drops the manager reference when the stream is closed.
type releasingReader struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *releasingReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// holders counts the live references across every cached generation of the
// project.
func (m *Manager) holders(projectID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.projectRefsLocked(projectID)
}

// BuildOptions assembles the SDK options for spec: the sandbox's daemon,
// image and limits, and the project's container, volume and environment.
func BuildOptions(spec Spec) (dockersb.Options, error) {
	opts, err := DaemonOptions(spec.Sandbox)
	if err != nil {
		return dockersb.Options{}, err
	}
	if err := applyImage(&opts, spec.Sandbox); err != nil {
		return dockersb.Options{}, err
	}
	if opts.Env, err = store.EnvMap(spec.Project.Env); err != nil {
		return dockersb.Options{}, fmt.Errorf("project %s: %w", spec.Project.Name, err)
	}
	opts.Persistent = true
	opts.KeepOnClose = true
	opts.TmpfsSize = "1g"
	opts.ContainerName = ContainerName(spec.Project.ID)
	opts.VolumeName = ProjectVolumeName(spec.Project.ID)
	return opts, nil
}

// DaemonOptions assembles the SDK options reaching the sandbox's daemon — how
// to talk to it, and nothing else. DOCKER ONLY: the managed-container calls
// take it as it stands, and a real build adds the image and the project.
// Anything reachable by more than one backend goes through Backend instead.
func DaemonOptions(sb *store.Sandbox) (dockersb.Options, error) {
	if sb.Type != "docker" {
		return dockersb.Options{}, fmt.Errorf("sandbox %q is a %s sandbox; this is a Docker-only operation", sb.Name, sb.Type)
	}
	var dc store.DockerConfig
	if err := store.DecodeConfig(sb.Config, &dc); err != nil {
		return dockersb.Options{}, fmt.Errorf("docker sandbox: invalid config: %w", err)
	}
	opts := dockersb.Options{Host: dc.Host}
	if strings.HasPrefix(dc.Host, "ssh://") {
		opts.SSH = dockersb.SSHAuth{
			UseAgent:              dc.SSHUseAgent,
			KeyFile:               dc.SSHKeyFile,
			Password:              dc.SSHPassword,
			KnownHostsFile:        dc.SSHKnownHosts,
			InsecureIgnoreHostKey: dc.SSHInsecureHostKey,
		}
	}
	return opts, nil
}

// DefaultContainerUser is what a sandbox that names no user runs as. Root,
// deliberately: the container is the isolation boundary, its files live in a
// volume nobody else mounts, and a workbench whose agent cannot install a
// package is a workbench that cannot do the work (decisions §5.33).
const DefaultContainerUser = "root"

// applyImage layers the image and container shape onto the daemon options.
func applyImage(opts *dockersb.Options, sb *store.Sandbox) error {
	var dc store.DockerConfig
	if err := store.DecodeConfig(sb.Config, &dc); err != nil {
		return fmt.Errorf("docker sandbox: invalid config: %w", err)
	}
	if dc.Image == "" {
		return fmt.Errorf("docker sandbox %s requires an image", sb.Name)
	}
	opts.Image = dc.Image
	opts.Runtime = dc.Runtime
	opts.User = dc.User
	if opts.User == "" {
		opts.User = DefaultContainerUser
	}
	opts.Network = dc.Network
	// A blank limit takes the workbench default, not "unlimited" (decisions
	// §5.38); an operator raises them per sandbox.
	mem := dc.MemoryMB
	if mem == 0 {
		mem = DefaultMemoryMB
	}
	cpus := dc.CPUs
	if cpus == 0 {
		cpus = DefaultCPUs
	}
	opts.Limits = sandbox.Limits{MemoryBytes: mem << 20, CPUs: cpus}
	opts.MaxReadFileBytes = dc.MaxReadFileBytes
	return nil
}

// Default resource caps for a docker sandbox that leaves them blank; 0 means
// this default, never "unlimited" (decisions §5.38).
const (
	DefaultMemoryMB int64   = 4096
	DefaultCPUs     float64 = 2
)

// shortID is a uuid's tail 12 hex chars — enough to tell ids apart in names
// docker must carry.
func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 12 {
		id = id[len(id)-12:]
	}
	return id
}

// ContainerName derives the docker container name serving a project.
// Deterministic, so a restarted server (or an idle-stopped container) is
// re-adopted by fingerprint instead of duplicated.
func ContainerName(projectID string) string {
	return "agents-" + shortID(projectID)
}

// ProjectVolumeName is the named volume serving a project's /workspace on its
// target's daemon (decisions §5.33).
func ProjectVolumeName(projectID string) string {
	return "agents-proj-" + shortID(projectID)
}
