// Package sandboxes keeps the live sandbox instances behind stored projects —
// built on demand, shared across a session, retired on edit — and the
// per-session command trust that gates exec_command.
package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
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
	// sandbox, so the next process finds the same one instead of provisioning
	// a second. The MANAGER fills it in before handing the spec to a backend —
	// callers never set it, and a backend that derives its own name (docker)
	// never calls it.
	SaveInstanceRef func(ctx context.Context, ref string) error
}

// sandboxKey identifies one live sandbox instance: the PROJECT it serves and
// that project's RUNTIME GENERATION. One axis is enough because the
// generation moves for every content change that can reach a container — the
// project's own, and the target's or template's through
// ProjectStore.BumpRuntimeGen (decisions §5.33). So a rename anywhere never
// splits the cache, and a content change anywhere always does.
type sandboxKey struct {
	projectID string
	gen       int64
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
	// doomed marks an instance evicted from the cache (a config edit, the
	// project's deletion, or the last bound session going away) while holders
	// remain: nothing new can acquire it, and the LAST release closes it — an
	// in-flight run or an open terminal finishes on the configuration it
	// started with instead of having its connection torn out from under it.
	// Guarded by mu.
	doomed bool
	// expired marks a stop in flight — an idle expiry or a user Stop: the
	// instance stays under its key so an acquire waits on gone instead of
	// adopting a stopping container (which would look dead and be
	// force-recreated, wiping its packages — decisions §5.28). gone closes when
	// the stop finished and the key is free. Guarded by mu.
	expired bool
	gone    chan struct{}
	// stopOnRelease pauses the compute when the last holder of a doomed
	// instance releases it — a user Stop deferred behind live work, or an idle
	// expiry — rather than only closing the connection, which for e2b releases
	// nothing remote. A retire, remove or rebuild leaves it false: a successor
	// generation keeps the sandbox, or a reclaim destroys it. Guarded by mu.
	stopOnRelease bool

	closeOnce sync.Once
}

// stopTimeout bounds a background pause/stop — an idle expiry or a deferred
// user Stop, neither of which has the caller's context to carry.
const stopTimeout = 30 * time.Second

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

// stop pauses the compute (Lifecycle.Stop) before releasing the connection —
// what an idle expiry or a deferred user Stop wants. For e2b that pause is the
// only thing that ends the billed sandbox (Close releases nothing remote); for
// docker it stops the container even when this instance never cached its id.
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
	// retired maps a project id to the lowest runtime generation still
	// current — the fence RetireProject moves on every content change that
	// reaches the project. An acquire that read the rows just before the
	// change builds from a retired generation; the fence makes that instance
	// doomed the moment its build lands, so it serves the run that started it
	// and closes, instead of living in the cache as a stale-credential
	// instance until process exit.
	retired map[string]int64
	// closed latches when CloseAll runs (process shutdown): no new acquire
	// may start, and every instance — building placeholders included — is
	// doomed so the last holder's release closes it.
	closed bool
	// buildOverride, when set (tests only), replaces buildSandbox — see
	// buildFn.
	buildOverride func(Spec) (sandbox.Sandbox, error)
	// trust holds per-session exec_command approval grants, consulted by the
	// commandGate and updated by the approval resolver.
	trust *TrustStore
	// writeInstanceRef persists a backend's handle on a project's sandbox.
	// Wired at bootstrap; without it a remote backend provisions a fresh
	// sandbox on every restart, so the build refuses rather than leaking one.
	writeInstanceRef func(ctx context.Context, projectID, ref string) error
	// idleAfter, when set, returns how long an unreferenced instance lives
	// before the idle-stop evicts it (0 = never). Read per release so a
	// settings change applies without a restart. The eviction closes the
	// instance; KeepOnClose makes that a container STOP, and the next
	// acquire re-adopts it (decisions §5.28).
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

// acquire backs Acquire, returning the instance itself.
//
// The build runs OUTSIDE the manager lock. An ssh dial can take seconds (the
// connect timeout defaults to 15s), and holding the lock through it would
// stall every other key — unrelated acquires, evictions, terminal opens —
// behind one unreachable host. The lock covers only the map: a first
// acquirer installs a placeholder and dials after unlocking; concurrent
// acquirers of the SAME key find the placeholder, take their reference, and
// wait on its ready gate — one dial, keyed contention only.
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
			// A failed build must not poison the key: the placeholder leaves
			// the cache so the next acquire retries with a fresh dial. Waiters
			// already holding the placeholder read buildErr off it. (The ==
			// guard: an eviction may have removed it — or a successor may
			// occupy the key — while the dial was in flight.)
			if m.instances[key] == inst {
				delete(m.instances, key)
			}
		case m.instances[key] != inst:
			// Evicted while dialing (a config edit's RetireProject, CloseAll,
			// the last bound session going). The evictor saw refs > 0 and
			// marked the placeholder doomed, so the last release closes what
			// was just built — nothing to do here; the case exists to say so.
		case m.retired[key.projectID] > key.gen || m.closed:
			// Built from a generation that was retired while the dial was in
			// flight (the acquire read the rows just before an update), or the
			// manager shut down meanwhile. Serve the holders that are already
			// waiting — their run validly started on this generation — but out
			// of the cache and doomed: the last release closes it, and no later
			// acquire can share an instance built from stale credentials or a
			// stale environment.
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
			// New work acquired the project after the deferred Stop was asked
			// for: pausing now would stop the container out from under it. The
			// stale stop is superseded; only this connection closes.
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

// idleExpire is the idle timer's body: evict and close the instance — unless
// a new holder arrived (the acquire disarms the timer, but a fire already in
// flight loses this race and must check), a successor replaced it under the
// key, or a user Stop already fenced it and owns its teardown.
func (m *Manager) idleExpire(inst *sandboxInstance) {
	m.mu.Lock()
	if m.instances[inst.key] != inst || inst.refs > 0 || inst.expired {
		m.mu.Unlock()
		return
	}
	// Fence BEFORE stopping: an acquire during the stop waits on gone rather
	// than adopting a container it would then judge dead and recreate from
	// scratch.
	inst.expired = true
	inst.gone = make(chan struct{})
	m.mu.Unlock()
	m.stopFenced(inst)
}

// stopFenced pauses inst's compute (not just the connection), then frees its
// key and its waiters. While the stop runs the instance stays under its key
// marked expired, so a racing Acquire waits on gone (see acquire); the caller
// set that fence under the lock.
func (m *Manager) stopFenced(inst *sandboxInstance) {
	inst.stop()
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

// RetireProject evicts every instance of a project built before minLive and
// fences that generation off, so an acquire whose build is still in flight
// cannot repopulate the cache with the old configuration. The eviction defers
// to live holders: a run or terminal already using an instance finishes on
// what it started with (see sandboxInstance.doomed).
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
// permanently (a random id is never reused): the project is gone, so a late
// build from a caller that read it before the delete cannot re-enter the cache
// with nothing left to retire it. In-flight holders finish on what they
// acquired; only idle instances close immediately.
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
// evicting the cached instance. Deleting a project deletes its files: the
// storage is what the row was for, and leaving it behind on every delete is
// an unbounded leak nobody has a listing for (decisions §5.33). The caller
// deletes the row first, so a failure here leaves reclaimable storage rather
// than a row pointing at nothing.
func (m *Manager) ReclaimProject(ctx context.Context, spec Spec) error {
	m.RemoveProject(spec.Project.ID)
	b, err := backendFor(spec)
	if err != nil {
		return err
	}
	return b.Reclaim(ctx, spec)
}

// RebuildContainer discards the project's compute and provisions it again
// from the current template and environment — the way back from a container
// someone broke. What "discard" means is the backend's: docker removes the
// container and keeps the volume, and a backend where the compute IS the
// storage refuses rather than destroying a working tree. In-flight commands
// in the old container fail; that is the deal a rebuild makes, and the caller
// warns before taking it.
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

// stopProjectOnRelease evicts every instance of the project and marks it so
// the last holder's release PAUSES the compute; an instance with no holders is
// paused now. Unlike EvictProject (a rebuild or unbind that only closes), this
// is a user Stop deferred behind live work — for e2b, pausing is the only way
// to end the billed sandbox.
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
		// No holders: stop it now, fenced under its key like the idle expiry,
		// so a racing Acquire waits instead of adopting the stopping container.
		// Disarm the idle timer; a fire already in flight no-ops on expired.
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
// apply_patch tools for the given project, holding a reference on the backing
// instance that the returned release drops (see Acquire — the caller releases
// when the run using the tools is over). apply_patch (Codex-style multi-file
// edits) and the file tools all edit through the same Sandbox, so they target
// the same filesystem exec_command runs in. Every container is persistent, so
// exec_command always offers named shells (session_id); they are scoped to
// this toolset, so the release also closes any the run opened. When
// commandApproval is set, exec_command is gated per call through the session
// command-trust store: a command is approved on first use, then trusted per
// the user's choice.
func (m *Manager) SandboxTools(spec Spec, commandApproval bool) ([]*agents.Tool, func(), error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return nil, nil, err
	}
	codeCfg := sandbox.CodeToolConfig{MaxOutputBytes: execToolMaxOutputBytes, Description: execToolDescription(spec)}
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

// execToolDescription appends the one thing a model cannot discover: a
// published port only reaches a server listening on 0.0.0.0, so a server bound
// to 127.0.0.1 is invisible to the preview however correctly it runs. Said
// only where it is actionable — where the project publishes something.
func execToolDescription(spec Spec) string {
	ports, err := store.DecodeProjectPorts(spec.Project.Ports)
	if err != nil || len(ports) == 0 {
		return ""
	}
	list := make([]string, 0, len(ports))
	for _, p := range ports {
		list = append(list, strconv.Itoa(p))
	}
	return sandbox.CodeToolConfig{}.DefaultDescription() +
		" This sandbox publishes port(s) " + strings.Join(list, ", ") +
		"; a server you start on one of them must listen on 0.0.0.0 (not 127.0.0.1) to be reachable from outside the container."
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
	// "Am I the only holder?" and "fence the instance as stopping" happen under
	// one lock: otherwise a concurrent Acquire slipping in between the count
	// and lc.Stop would build against the very container this call stops.
	// The fence is the idle expiry's (expired/gone): the racing Acquire waits
	// out the stop; finish then frees the keys, and this call's own release
	// closes what it detached.
	finish, sole := m.detachIfSole(spec.Project.ID)
	if !sole {
		// Someone is still working in it; leave the container up and let the
		// last holder's release PAUSE it — close alone releases nothing remote
		// for e2b, so a plain eviction would never stop the billed sandbox.
		m.stopProjectOnRelease(spec.Project.ID)
		return false, nil
	}
	err = lc.Stop(ctx)
	finish()
	return err == nil, err
}

// detachIfSole reports whether this caller holds the project's only reference,
// and if so dooms every instance of the project and fences each as stopping
// (expired + gone) WITHOUT freeing its key, so a racing Acquire waits for the
// stop to complete instead of adopting a container mid-stop — idleExpire's
// shape. The returned finish frees the keys and the waiters; the caller runs
// it after Lifecycle.Stop, error or not. Callers hold one reference (from
// Acquire).
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
	rc, err := ex.ExportTar(ctx, "")
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

// previewDialTimeout bounds one preview connection attempt.
const previewDialTimeout = 5 * time.Second

// Preview resolves where a port inside the project's sandbox answers, and how
// to reach it: the URL a proxy forwards to, and — for a backend whose ports
// are not routable from this process — the dial that gets there. The returned
// release drops the instance reference; the caller holds it for the proxied
// request's lifetime, so an eviction cannot close the connection mid-response.
func (m *Manager) Preview(ctx context.Context, spec Spec, port int) (target string, dial DialFunc, release func(), err error) {
	sb, release, err := m.Acquire(spec)
	if err != nil {
		return "", nil, nil, err
	}
	fwd, ok := sb.(sandbox.PortForwarder)
	if !ok {
		release()
		return "", nil, nil, fmt.Errorf("%s sandbox: cannot expose a port", spec.Sandbox.Type)
	}
	target, err = fwd.URLForPort(ctx, port)
	if err != nil {
		release()
		return "", nil, nil, err
	}
	if d, ok := sb.(sandbox.PortDialer); ok {
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Bounded: an unpublished port on a network this host cannot route
			// to does not refuse, it hangs — and a preview that spins for a
			// TCP timeout tells a person nothing. The error handler explains.
			ctx, cancel := context.WithTimeout(ctx, previewDialTimeout)
			defer cancel()
			return d.DialPort(ctx, port)
		}
	}
	return target, dial, release, nil
}

// DialFunc is an http.Transport's DialContext, which is what a proxy needs
// from a backend that opens its own connections.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

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
	if opts.Ports, err = store.DecodeProjectPorts(spec.Project.Ports); err != nil {
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
	if err := unmarshalConfig(sb.Config, &dc); err != nil {
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
	if err := unmarshalConfig(sb.Config, &dc); err != nil {
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
	// A blank limit takes the workbench default, not "unlimited": agent-
	// generated code runs here, so an uncapped container could exhaust the
	// host's memory or CPU. An operator raises them per sandbox.
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

// Default resource caps for a workbench docker sandbox that leaves them blank.
// 0 in the config means "this default", never "unlimited" — an unbounded
// container running agent code is a host-DoS surface (decisions §5.38).
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

// unmarshalConfig decodes a stored Config payload, treating empty as a
// zero-value config so the per-type required-field checks produce the error.
func unmarshalConfig(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
