// Package docker implements the sandbox.Sandbox interface using Docker
// containers. Two modes are supported:
//
// - Ephemeral (default): each Exec creates a fresh container, runs the
// command, captures output and removes the container.
// - Persistent (Options.Persistent = true): a single long-lived container is
// started on the first Exec; subsequent Exec calls use "docker exec" to run
// commands inside it. The container is removed on Close.
//
// The package is split by concern: this file holds the sandbox type and the
// Exec family, files.go the file operations with their two backends
// (files_host.go for a bind-mounted host directory, files_container.go for the
// in-container one) and tar.go the tar packing shared by both.
//
// This package pulls the (heavy) Docker client; it is a separate module so the
// core agents-go module stays dependency-light.
package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

const (
	// workDir is the working directory inside the container, matching the
	// Dockerfile WORKDIR. An anonymous volume is mounted here in ephemeral
	// mode so files can be copied in before start on a read-only root fs.
	workDir = "/workspace"
	// logReadTimeout bounds reading the container logs after a timeout, when
	// the request's own context is already spent.
	logReadTimeout = 10 * time.Second
	// logMaxSize caps the json-file container log on the daemon's disk so a
	// flooding command cannot fill the host filesystem. Output returned to the
	// caller is capped separately (ExecRequest.MaxOutputBytes), and well below
	// this.
	logMaxSize = "10m"
	// fingerprintLabel marks a persistent container as created by this package,
	// carrying a hash of every security-relevant option it was created from.
	// adoptNamed requires an exact match: a name conflict may only be resolved
	// by taking over a container this package created from the SAME
	// configuration — never a foreign container, and never one created under a
	// laxer policy (network on, root user, no limits) that the current options
	// no longer allow.
	fingerprintLabel = "dev.agents-go.sandbox.fingerprint"
)

// configFingerprint hashes the options that decide what a persistent container
// IS security-wise: image, runtime, user, network, the published ports, the
// bind source, and the resource limits. ContainerWorkDir is deliberately excluded — persistent mode
// passes the working directory per exec, so it does not change the container.
// Effective values are hashed, not raw ones (the PIDs default is applied here
// as in buildHostConfig), so equivalent configurations produce one
// fingerprint.
func (s *Sandbox) configFingerprint() string {
	pids := s.opts.Limits.PIDs
	if pids == 0 {
		pids = 128
	}
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\nruntime=%s\nuser=%s\nnetwork=%s\nworkdir=%s\nvolume=%s\ntmpfs=%s\nmemory=%d\ncpus=%v\npids=%d\n",
		s.opts.Image, s.opts.Runtime, s.opts.User, s.opts.Network,
		filepath.Clean(s.opts.WorkDir), s.opts.VolumeName, s.tmpfsSize(),
		s.opts.Limits.MemoryBytes, s.opts.Limits.CPUs, pids)
	// Written only when set: an unconditional line would change every
	// existing container's fingerprint and force a fleet-wide replace.
	if env := envSlice(s.opts.Env); len(env) > 0 {
		fmt.Fprintf(h, "env=%s\n", strings.Join(env, "\x00"))
	}
	if ports := s.publishedPorts(); len(ports) > 0 {
		fmt.Fprintf(h, "ports=%v\n", ports)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// publishedPorts is Options.Ports canonicalized: in range, deduplicated and
// ordered, so one list produces one fingerprint however it was written.
func (s *Sandbox) publishedPorts() []int {
	if len(s.opts.Ports) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(s.opts.Ports))
	out := make([]int, 0, len(s.opts.Ports))
	for _, p := range s.opts.Ports {
		if p <= 0 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Options configures the Docker sandbox.
type Options struct {
	// Image is the container image to run. Required.
	Image string
	// Host is the Docker daemon address. Empty uses the standard DOCKER_HOST
	// environment variable (or the platform default socket); "tcp://host:port"
	// reaches a TCP daemon; "ssh://user@host[:port][/socket]" reaches a remote
	// daemon's unix socket through SSH (pure Go — see SSHAuth).
	Host string
	// SSH configures authentication and host-key verification for an ssh://
	// Host; ignored otherwise.
	SSH SSHAuth
	// Runtime is the OCI runtime to use (e.g. "runsc" for gVisor). When empty,
	// the daemon's default runtime (usually runc) is used.
	Runtime string
	// Limits caps the container's resources. Zero fields use the defaults below.
	Limits sandbox.Limits
	// User runs the process as the given user[:group]. Empty keeps the image's
	// own user, which for most images is root — the user a container needs to
	// be able to install packages into itself.
	User string
	// Ports are published from the container to the DAEMON's loopback on
	// ephemeral host ports, so a service inside is reachable without putting
	// it on any other interface. Part of the fingerprint: changing the list
	// replaces a persistent container. A published port only reaches a
	// process listening on the container's network interface — a server bound
	// to 127.0.0.1 inside is not reachable through it (spec §2.7r).
	Ports []int
	// Network names the docker network the container joins. Empty means "none"
	// — no network at all, the default. "default" or "bridge" gives the
	// daemon's ordinary networking; a user-defined network name puts the
	// container where other containers (and the host process that created it)
	// can reach it by name.
	Network string
	// Env sets environment variables on the CONTAINER, so every command,
	// shell and terminal in it sees them. An ExecRequest.Env entry of the
	// same name wins for that one call. Part of the fingerprint: changing it
	// replaces a persistent container rather than adopting the old one.
	Env map[string]string
	// Persistent, when true, keeps a single container alive across Exec calls
	// instead of creating and destroying one per call.
	Persistent bool
	// ContainerName sets the Docker container name in persistent mode. Ignored
	// in ephemeral mode (containers are unnamed). When empty a random name is
	// assigned by the daemon.
	ContainerName string
	// WorkDir is a host directory to bind-mount into the container's /workspace.
	// When set, it replaces the anonymous volume so the container sees (and can
	// modify) the host files directly. Typically used with Persistent mode.
	WorkDir string
	// VolumeName mounts the named Docker volume at /workspace instead of a
	// host directory or an anonymous volume — durable storage on a REMOTE
	// daemon, where a host path means nothing. Ignored when WorkDir is set.
	VolumeName string
	// TmpfsSize is the /tmp tmpfs size (e.g. "1g"); empty = "64m". RAM-backed.
	TmpfsSize string
	// KeepOnClose leaves the persistent container (stopped, not removed) on
	// Close, so a later Sandbox with the same ContainerName and configuration
	// adopts it — packages and files in the container survive process
	// restarts and idle teardowns. Ignored in ephemeral mode.
	KeepOnClose bool
	// ContainerWorkDir is the working directory commands run in INSIDE the
	// container: /workspace itself (the default when empty) or a subdirectory
	// of it — the mount point never moves, but a session may work in one
	// project subtree of the mounted directory. Anything outside /workspace is
	// rejected by New. Relative paths given to the file tools resolve against
	// it, keeping them in the same view as exec.
	ContainerWorkDir string
	// MaxReadFileBytes caps how many bytes ReadFile returns; larger files fail
	// with sandbox.ErrReadLimitExceeded instead of being loaded into host
	// memory. Zero (or negative) means sandbox.DefaultMaxReadFileBytes.
	MaxReadFileBytes int64
}

// tmpfsSize is the /tmp tmpfs size option: Options.TmpfsSize or the 64m default.
func (s *Sandbox) tmpfsSize() string {
	if s.opts.TmpfsSize != "" {
		return s.opts.TmpfsSize
	}
	return "64m"
}

// containerWorkDir is the directory commands run in inside the container:
// Options.ContainerWorkDir (validated by New to be /workspace or below it),
// or the /workspace mount point itself.
func (s *Sandbox) containerWorkDir() string {
	if s.opts.ContainerWorkDir != "" {
		return s.opts.ContainerWorkDir
	}
	return workDir
}

// subDir is containerWorkDir relative to the /workspace mount point ("" when
// they coincide) — the offset the host-side file operations apply so relative
// paths resolve in the same directory exec runs in.
func (s *Sandbox) subDir() string {
	return strings.TrimPrefix(strings.TrimPrefix(s.containerWorkDir(), workDir), "/")
}

// Sandbox is a Docker-backed sandbox.Sandbox.
type Sandbox struct {
	cli  *client.Client
	opts Options

	// image pull state
	pullMu   sync.Mutex
	pullDone bool

	// sshDial is set for an ssh:// Host and closed with the sandbox.
	sshDial *sshDialer

	// persistent container state
	mu          sync.Mutex
	containerID string
}

// New connects to the Docker daemon (via the standard environment variables) and
// returns a Sandbox running opts.Image.
func New(opts Options) (*Sandbox, error) {
	if opts.Image == "" {
		return nil, fmt.Errorf("docker sandbox: Image is required")
	}
	// A WorkDir bind is resolved by the DAEMON's filesystem while the file
	// tools and the workdir mkdir run on THIS host — with a remote daemon the
	// two silently diverge. Use VolumeName for remote daemons.
	if opts.WorkDir != "" && opts.Host != "" {
		return nil, fmt.Errorf("docker sandbox: WorkDir needs the local daemon; use VolumeName with Host %q", opts.Host)
	}
	if opts.ContainerWorkDir != "" {
		clean := path.Clean(opts.ContainerWorkDir)
		if clean != workDir && !strings.HasPrefix(clean, workDir+"/") {
			return nil, fmt.Errorf("docker sandbox: ContainerWorkDir %q must be %s or a subdirectory of it", opts.ContainerWorkDir, workDir)
		}
		opts.ContainerWorkDir = clean
	}
	// API-version negotiation is on by default in the moby client.
	clientOpts := []client.Opt{client.FromEnv}
	var sshDial *sshDialer
	switch {
	case strings.HasPrefix(opts.Host, "ssh://"):
		d, err := newSSHDialer(opts.Host, opts.SSH)
		if err != nil {
			return nil, err
		}
		sshDial = d
		// The host is a placeholder: every request rides the dialer's channel
		// to the remote socket (the connhelper pattern).
		clientOpts = append(clientOpts, client.WithHost("http://docker.invalid"), client.WithDialContext(d.DialContext))
	case opts.Host != "":
		clientOpts = append(clientOpts, client.WithHost(opts.Host))
	}
	cli, err := client.New(clientOpts...)
	if err != nil {
		if sshDial != nil {
			_ = sshDial.Close()
		}
		return nil, fmt.Errorf("docker sandbox: %w", err)
	}
	return &Sandbox{cli: cli, opts: opts, sshDial: sshDial}, nil
}

// ensureImage pulls the image if it is not available locally. Unlike sync.Once,
// a transient pull failure does not permanently latch — the next call retries.
func (s *Sandbox) ensureImage(ctx context.Context) error {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	if s.pullDone {
		return nil
	}
	if _, err := s.cli.ImageInspect(ctx, s.opts.Image); err == nil {
		s.pullDone = true
		return nil
	}
	resp, err := s.cli.ImagePull(ctx, s.opts.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("docker sandbox: pull %s: %w", s.opts.Image, err)
	}
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("docker sandbox: pull %s: %w", s.opts.Image, err)
	}
	s.pullDone = true
	return nil
}

// ensureContainer lazily creates and starts the persistent container, or
// recreates it if the existing one has exited (e.g. OOM-killed).
//
// Creation runs while s.mu is held — three daemon round-trips (create, copy,
// start) under the lock. That is deliberate: it is what guarantees a single
// container, and the alternative (create outside the lock, keep the winner)
// would leave the losers' containers to be reaped. The cost is that a wedged
// daemon blocks every other s.mu user — Close and, in persistent mode, all the
// file operations — for as long as the create hangs.
func (s *Sandbox) ensureContainer(ctx context.Context) (string, error) {
	id, err := s.lookupRunning(ctx)
	if err != nil || id != "" {
		return id, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.containerID != "" {
		// Another caller created one while we were looking.
		return s.containerID, nil
	}
	return s.createContainer(ctx)
}

// lookupRunning returns the persistent container's ID when one is known AND
// still running. A container that is positively gone (inspected: not running,
// or not found) is force-removed, forgotten, and reported as "" so the caller
// creates a fresh one. The inspect runs without s.mu: it is a daemon round-trip
// and every Exec passes through here.
func (s *Sandbox) lookupRunning(ctx context.Context) (string, error) {
	s.mu.Lock()
	id := s.containerID
	s.mu.Unlock()
	if id == "" {
		return "", nil
	}

	info, err := s.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	switch {
	case err == nil && info.Container.State != nil && info.Container.State.Running:
		return id, nil
	case err != nil && !cerrdefs.IsNotFound(err):
		// A failure to LOOK is not "the container is dead": retiring one on
		// a daemon hiccup, or merely because the caller's ctx was already
		// cancelled, destroys a healthy container's packages, processes and
		// workspace volume. Only a positive answer (inspected: not running
		// / not found) may retire it.
		return "", fmt.Errorf("inspecting persistent container: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.containerID == id {
		// WithoutCancel: a canceled ctx would skip the remove while the ID
		// is dropped below, leaking the dead container forever.
		_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		s.containerID = ""
	}
	return "", nil
}

// createContainer creates, seeds and starts the persistent container. It must
// be called with s.mu held; on success s.containerID is the new ID.
func (s *Sandbox) createContainer(ctx context.Context) (string, error) {
	if s.opts.WorkDir != "" {
		// Linux dockerd auto-creates a missing bind source; Docker Desktop
		// (macOS/Windows) refuses with "bind source path does not exist".
		// Create it up front so behavior does not depend on the platform.
		if err := os.MkdirAll(s.opts.WorkDir, 0o755); err != nil {
			return "", fmt.Errorf("docker sandbox: creating bind source %s: %w", s.opts.WorkDir, err)
		}
	}
	cfg, hostCfg := s.buildPersistentConfig()
	createOpts := client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, Name: s.opts.ContainerName}
	created, err := s.cli.ContainerCreate(ctx, createOpts)
	if err != nil {
		// A fixed ContainerName can collide with a container WE left behind —
		// a previous process run, or another Sandbox instance sharing the
		// name. Adopt it when it matches what we would have created; replace
		// it when it is ours from an older configuration (a config edit);
		// a foreign holder stays a hard error.
		if s.opts.ContainerName != "" && cerrdefs.IsConflict(err) {
			id, aerr := s.adoptNamed(ctx)
			if errors.Is(aerr, errStaleOurs) {
				// Remove by the INSPECTED id — the name could have changed
				// hands since. RemoveVolumes reaps only the ANONYMOUS
				// /workspace volume — docker never auto-removes named ones —
				// so a replace leaks nothing and drops nothing a project keeps.
				_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
				created2, cerr := s.cli.ContainerCreate(ctx, createOpts)
				if cerr != nil {
					return "", fmt.Errorf("docker sandbox: recreate after config change: %w", cerr)
				}
				created, err = created2, nil
			} else if aerr != nil {
				return "", fmt.Errorf("docker sandbox: create: %w (name %q held by an incompatible container: %v)", err, s.opts.ContainerName, aerr)
			} else {
				s.containerID = id
				return id, nil
			}
		} else {
			return "", fmt.Errorf("docker sandbox: create: %w", err)
		}
	}
	id := created.ID

	// Cleanup must not use ctx directly: when the failure was caused by ctx
	// being canceled (or timing out), a ctx-bound remove would fail too and
	// leak the container.
	rmCtx := context.WithoutCancel(ctx)
	remove := func() {
		_, _ = s.cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}
	tarball, terr := buildTar(nil)
	if terr != nil {
		remove()
		return "", terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		remove()
		return "", fmt.Errorf("docker sandbox: copy files: %w", err)
	}
	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		remove()
		return "", fmt.Errorf("docker sandbox: start: %w", err)
	}
	s.containerID = id
	return id, nil
}

// adoptNamed takes over the existing container holding our fixed name,
// provided the fingerprint label proves it OURS from the SAME configuration
// (decisions §5.19). A stopped match is started; ours-from-an-older-config is
// errStaleOurs with the stale container's id, so the caller replaces exactly
// the container it judged; a foreign holder is a hard error.
func (s *Sandbox) adoptNamed(ctx context.Context) (string, error) {
	info, err := s.cli.ContainerInspect(ctx, s.opts.ContainerName, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	c := info.Container
	// Ownership first: a container without our label is FOREIGN and stays a
	// hard error; one with our label but a different fingerprint is a stale
	// build of ours (a config edit since it was created) and is replaced.
	label := ""
	if c.Config != nil {
		label = c.Config.Labels[fingerprintLabel]
	}
	switch label {
	case s.configFingerprint():
		// Ours, same configuration.
	case "":
		return "", fmt.Errorf("it was not created by this sandbox (no %s label); remove or rename the container", fingerprintLabel)
	default:
		return c.ID, errStaleOurs // the id rides along so the replace acts on IT, not the name
	}
	// Belt to the fingerprint's braces on the two fields a drifted daemon
	// could disagree about.
	if c.Config.Image != s.opts.Image {
		return c.ID, errStaleOurs
	}
	if s.opts.WorkDir != "" {
		src := ""
		for _, m := range c.Mounts {
			if m.Destination == workDir {
				src = m.Source
				break
			}
		}
		if filepath.Clean(src) != filepath.Clean(s.opts.WorkDir) {
			return c.ID, errStaleOurs
		}
	}
	if c.State == nil || !c.State.Running {
		if _, err := s.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			return "", fmt.Errorf("starting it: %w", err)
		}
	}
	return c.ID, nil
}

// errStaleOurs marks a name conflict with a container THIS package created
// from an older configuration: safe to replace, unlike a foreign holder.
var errStaleOurs = errors.New("held by our container from a different configuration")

// Exec implements sandbox.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if err := s.prepareExec(ctx, req); err != nil {
		return nil, err
	}
	if !s.opts.Persistent {
		// Ephemeral mode reads the container log once the process has exited,
		// so it fills the result's Stdout/Stderr itself.
		return s.execEphemeral(ctx, req)
	}
	// Persistent mode has a single core shared with ExecStream: capped buffers
	// stand in for the caller's writers, as in the local and ssh backends.
	maxOut := req.EffectiveMaxOutputBytes()
	stdout := &sandbox.CappedBuffer{Max: maxOut}
	stderr := &sandbox.CappedBuffer{Max: maxOut}
	res, err := s.execPersistent(ctx, req, stdout, stderr)
	if err != nil {
		return nil, err
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res, nil
}

// ExecStream implements sandbox.ExecStreamer.
func (s *Sandbox) ExecStream(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	if err := s.prepareExec(ctx, req); err != nil {
		return nil, err
	}
	if s.opts.Persistent {
		return s.execPersistent(ctx, req, stdout, stderr)
	}
	// Ephemeral mode is the one place Exec and ExecStream cannot share a core:
	// Exec collects the log the daemon already stored once the process exited,
	// while streaming has to follow the log live (see streamEphemeral).
	return s.streamEphemeral(ctx, req, stdout, stderr)
}

// prepareExec makes the image available and rejects requests no mode supports.
func (s *Sandbox) prepareExec(ctx context.Context, req sandbox.ExecRequest) error {
	if err := s.ensureImage(ctx); err != nil {
		return err
	}
	if req.Stdin != "" {
		return fmt.Errorf("docker sandbox: ExecRequest.Stdin is not supported")
	}
	if len(req.Cmd) == 0 {
		return fmt.Errorf("docker sandbox: ExecRequest.Cmd is empty")
	}
	return nil
}

// execPersistent runs req inside the long-lived container and writes the
// demultiplexed output to stdout/stderr. It is the shared core of Exec (which
// passes capped buffers) and ExecStream (which passes the caller's writers), so
// the returned ExecResult never carries Stdout/Stderr of its own.
//
// Because the writers are the caller's, one that refuses a write aborts the
// call with that error rather than yielding an exit code — see copyAttached for
// why a stream that ended early cannot be trusted here.
func (s *Sandbox) execPersistent(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	id, err := s.ensureContainer(ctx)
	if err != nil {
		return nil, err
	}

	if len(req.Files) > 0 {
		tarball, terr := buildTar(req.Files)
		if terr != nil {
			return nil, terr
		}
		if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
			return nil, fmt.Errorf("docker sandbox: copy files: %w", err)
		}
	}

	// Tag the exec so killExec can find it inside the container on timeout;
	// appended last so a request Env cannot override it.
	marker := newExecMarker()
	execOpts := client.ExecCreateOptions{
		Cmd:          req.Cmd,
		WorkingDir:   s.containerWorkDir(),
		Env:          append(envSlice(req.Env), execMarkerEnv+"="+marker),
		AttachStdout: true,
		AttachStderr: true,
	}
	created, err := s.cli.ExecCreate(ctx, id, execOpts)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec create: %w", err)
	}

	ectx, cancel := context.WithTimeout(ctx, req.EffectiveTimeout())
	defer cancel()

	attached, err := s.cli.ExecAttach(ectx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec attach: %w", err)
	}
	defer attached.Close()

	cerr := copyAttached(ectx, attached.Reader, attached.Close, stdout, stderr)

	res := &sandbox.ExecResult{}
	// The CALLER's ending is checked first. ectx inherits it, deadline included,
	// so asking ectx first would report a deadline the caller set as this
	// command's own timeout — and TimedOut means "killed for exceeding the
	// request timeout", nothing else (spec §2.7m).
	if err := ctx.Err(); err != nil {
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return nil, err
	}
	if ectx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		// Kill the exec process so it doesn't linger in the container.
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return res, nil
	}
	if cerr != nil {
		// The stream broke with the process still running, and this container
		// outlives the call — leaving it would leak a process per failure.
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return nil, fmt.Errorf("docker sandbox: exec read: %w", cerr)
	}

	// The stream ended on its own, so the process is done and its exit code is
	// final. (The read is never cut short while the process still runs — see
	// copyAttached.)
	inspect, ierr := s.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if ierr != nil {
		return nil, fmt.Errorf("docker sandbox: exec inspect: %w", ierr)
	}
	res.ExitCode = inspect.ExitCode
	return res, nil
}

// copyAttached demultiplexes an exec attach stream into stdout/stderr and
// returns once the copy is finished — never before, so nothing writes to the
// sinks after it returns.
//
// The attach connection is a hijacked raw net.Conn: the context that bounded
// the attach handshake does NOT interrupt reads on it. So the copy runs in a
// goroutine and, when ctx fires, sever closes the connection to unblock the
// read; otherwise a command that never exits (e.g. "sleep infinity") would hang
// the call forever.
//
// Reading continues to the end of the stream even once the sinks stop keeping
// bytes: on a live attach "no more output wanted" is not "the process exited".
// Ending the read at a full sink would hand a still-running exec to
// ExecInspect, which reports ExitCode 0 for it: a command that flooded both
// streams would come back as a clean exit while it kept running.
// Memory stays bounded because the sinks discard beyond their cap.
//
// A frame cut in half surfaces as ErrUnexpectedEOF, which only means the output
// was truncated; it is reported as success. Every other error is real (a broken
// stream, or a sink that refused a write) and is returned, which fails the whole
// call: the stream ending is what says the process finished, so once it ended
// for another reason ExecInspect's exit code is not an answer about this
// command. The ephemeral side drops the same failure on purpose — its exit
// status comes from ContainerWait, so a broken log copy costs only output
// (see streamEphemeral).
func copyAttached(ctx context.Context, r io.Reader, sever func(), stdout, stderr io.Writer) error {
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, r)
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		sever()
		err = <-done
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	return nil
}

// execEphemeral runs req in a throw-away container and returns its buffered
// output. Unlike the persistent core it fills the result's Stdout/Stderr: the
// container log is read once, after the process has exited.
func (s *Sandbox) execEphemeral(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	id, remove, err := s.startEphemeral(ctx, req)
	if err != nil {
		return nil, err
	}
	defer remove()

	timeout := req.EffectiveTimeout()
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &sandbox.ExecResult{}
	wait := s.cli.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	statusCh, errCh := wait.Result, wait.Error
	select {
	case status := <-statusCh:
		if status.Error != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %s", status.Error.Message)
		}
		res.ExitCode = int(status.StatusCode)
	case werr := <-errCh:
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if wctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.ExitCode = -1
			_, _ = s.cli.ContainerKill(context.WithoutCancel(ctx), id, client.ContainerKillOptions{Signal: "KILL"})
		} else if werr != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %w", werr)
		}
	}

	lctx := ctx
	if res.TimedOut {
		var lcancel context.CancelFunc
		lctx, lcancel = context.WithTimeout(context.WithoutCancel(ctx), logReadTimeout)
		defer lcancel()
	}
	stdout, stderr, lerr := s.readLogs(lctx, id, req.EffectiveMaxOutputBytes())
	if lerr != nil {
		return nil, lerr
	}
	res.Stdout, res.Stderr = stdout, stderr
	return res, nil
}

// streamEphemeral runs req in a throw-away container, following its log so the
// writers see output as it is produced.
func (s *Sandbox) streamEphemeral(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	id, remove, err := s.startEphemeral(ctx, req)
	if err != nil {
		return nil, err
	}
	defer remove()

	timeout := req.EffectiveTimeout()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rc, err := s.cli.ContainerLogs(tctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: logs: %w", err)
	}
	defer rc.Close()

	// A copy error here costs output, not correctness, so it is dropped: the
	// exit status comes from ContainerWait below, which does not depend on the
	// log stream at all. (The persistent core cannot be as relaxed — see
	// copyAttached.)
	_, _ = stdcopy.StdCopy(stdout, stderr, rc)

	res := &sandbox.ExecResult{}
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		_, _ = s.cli.ContainerKill(context.WithoutCancel(ctx), id, client.ContainerKillOptions{Signal: "KILL"})
		return res, nil
	}

	wait := s.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case status := <-wait.Result:
		if status.Error != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %s", status.Error.Message)
		}
		res.ExitCode = int(status.StatusCode)
	case werr := <-wait.Error:
		if werr != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %w", werr)
		}
	}
	return res, nil
}

// startEphemeral creates a throw-away container for req, seeds its working
// directory and starts it. The returned remove function must be called by the
// caller (it also runs on the failure paths of the wait/read that follow).
func (s *Sandbox) startEphemeral(ctx context.Context, req sandbox.ExecRequest) (string, func(), error) {
	cfg, hostCfg := s.buildConfig(req)
	created, err := s.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return "", nil, fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID
	remove := func() {
		_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}

	// Always sent, even with no request files: the tarball also creates the
	// writable working directory inside the root-owned volume.
	tarball, terr := buildTar(req.Files)
	if terr != nil {
		remove()
		return "", nil, terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		remove()
		return "", nil, fmt.Errorf("docker sandbox: copy files: %w", err)
	}
	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		remove()
		return "", nil, fmt.Errorf("docker sandbox: start: %w", err)
	}
	return id, remove, nil
}

// buildHostConfig returns the HostConfig and, for a persistent container, the
// set of ports it exposes (empty otherwise). Persistent mode relaxes the
// read-only root filesystem, so a container whose User can write to it can
// install packages into itself.
func (s *Sandbox) buildHostConfig(persistent bool) (*container.HostConfig, network.PortSet) {
	netMode := container.NetworkMode("none")
	if s.opts.Network != "" {
		netMode = container.NetworkMode(s.opts.Network)
	}
	var mounts []mount.Mount
	switch {
	case s.opts.WorkDir != "":
		mounts = []mount.Mount{{Type: mount.TypeBind, Source: s.opts.WorkDir, Target: workDir}}
	case s.opts.VolumeName != "":
		mounts = []mount.Mount{{Type: mount.TypeVolume, Source: s.opts.VolumeName, Target: workDir}}
	default:
		mounts = []mount.Mount{{Type: mount.TypeVolume, Target: workDir}}
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    netMode,
		Runtime:        s.opts.Runtime,
		ReadonlyRootfs: !persistent,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Mounts:         mounts,
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,size=" + s.tmpfsSize() + ",mode=1777"},
		// Cap the container log on disk: without this, a flooding command
		// ("yes" and friends) can write gigabytes into the daemon's log
		// directory within a single timeout window.
		LogConfig: container.LogConfig{
			Type:   "json-file",
			Config: map[string]string{"max-size": logMaxSize, "max-file": "1"},
		},
	}
	// Ports are published only for the persistent container: an ephemeral
	// one-shot has nothing to serve between commands, and publishing is decided
	// once, at create (spec §2.7r). Returning the set keeps it off a shared
	// field that concurrent ephemeral Execs would race on.
	var exposed network.PortSet
	if ports := s.publishedPorts(); persistent && len(ports) > 0 {
		exposed = network.PortSet{}
		bindings := network.PortMap{}
		for _, p := range ports {
			port, err := network.ParsePort(strconv.Itoa(p) + "/tcp")
			if err != nil {
				continue // publishedPorts already dropped anything out of range
			}
			exposed[port] = struct{}{}
			// Host port 0 = the daemon picks a free one; loopback so the
			// service reaches no other interface of the daemon's host.
			bindings[port] = []network.PortBinding{{HostIP: netip.AddrFrom4([4]byte{127, 0, 0, 1}), HostPort: "0"}}
		}
		hostCfg.PortBindings = bindings
	}
	hostCfg.Memory = s.opts.Limits.MemoryBytes
	if s.opts.Limits.CPUs > 0 {
		hostCfg.NanoCPUs = int64(s.opts.Limits.CPUs * 1e9)
	}
	pids := s.opts.Limits.PIDs
	if pids == 0 {
		pids = 128
	}
	hostCfg.PidsLimit = &pids
	return hostCfg, exposed
}

// buildConfig assembles the container and host configuration for ephemeral mode.
func (s *Sandbox) buildConfig(req sandbox.ExecRequest) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.opts.Image,
		Entrypoint: req.Cmd,
		WorkingDir: s.containerWorkDir(),
		Env:        s.containerEnv(req.Env),
		Tty:        false,
	}
	cfg.User = s.opts.User
	hostCfg, _ := s.buildHostConfig(false)
	return cfg, hostCfg
}

// buildPersistentConfig assembles the container and host configuration for
// persistent mode. The container runs "sleep infinity" so it stays alive.
func (s *Sandbox) buildPersistentConfig() (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.opts.Image,
		Entrypoint: []string{"sleep", "infinity"},
		WorkingDir: s.containerWorkDir(),
		Env:        envSlice(s.opts.Env),
		Tty:        false,
		// The ownership stamp adoptNamed verifies before taking over a
		// same-named container (see fingerprintLabel).
		Labels: map[string]string{fingerprintLabel: s.configFingerprint()},
	}
	cfg.User = s.opts.User
	// The container config declares the same ports the host config binds.
	hostCfg, exposed := s.buildHostConfig(true)
	cfg.ExposedPorts = exposed
	return cfg, hostCfg
}

// readLogs fetches the container output, capping each stream at max bytes.
func (s *Sandbox) readLogs(ctx context.Context, id string, maxBytes int64) (string, string, error) {
	rc, err := s.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", "", fmt.Errorf("docker sandbox: logs: %w", err)
	}
	defer rc.Close()
	stdout, stderr, derr := demuxLogs(rc, maxBytes)
	if derr != nil {
		return "", "", fmt.Errorf("docker sandbox: read logs: %w", derr)
	}
	return stdout, stderr, nil
}

// demuxLogs splits a multiplexed docker log stream into stdout and stderr,
// capping each at max bytes. Reading stops at the source only once BOTH
// streams are full — a per-total limit would let a flooding stdout starve a
// short stderr (or vice versa) that arrives later in the stream. The flip
// side: a single flooding stream does NOT end the read early; its excess is
// read and discarded until the other stream also fills or the source ends.
// Memory stays bounded throughout because each buffer discards beyond its cap.
//
// This reads a FINISHED log, which is why stopping early is safe here: the
// process has already exited and the stream is finite. A live stream (the
// persistent-mode attach) must be read to its end instead — see copyAttached.
//
// The capped output collected so far is returned even when err is non-nil,
// so a timed-out caller can surface the partial output.
func demuxLogs(r io.Reader, maxBytes int64) (string, string, error) {
	stdout := &sandbox.CappedBuffer{Max: maxBytes}
	stderr := &sandbox.CappedBuffer{Max: maxBytes}
	src := &stopWhenFull{r: r, full: func() bool { return stdout.Full() && stderr.Full() }}
	// A cut mid-frame surfaces as ErrUnexpectedEOF, which just means "truncated".
	if _, err := stdcopy.StdCopy(stdout, stderr, src); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// stopWhenFull reads from r until full() reports both sinks are at capacity,
// then ends the stream early.
type stopWhenFull struct {
	r    io.Reader
	full func() bool
}

func (s *stopWhenFull) Read(p []byte) (int, error) {
	if s.full() {
		return 0, io.EOF
	}
	return s.r.Read(p)
}

// execMarkerEnv tags each persistent-mode exec with a unique value so a
// timed-out process can be found from inside the container. ExecInspect's
// PID is a HOST-namespace PID, which is meaningless in the container's PID
// namespace, so killing by inspected PID silently does nothing.
const execMarkerEnv = "AGENTS_SANDBOX_EXEC"

// newExecMarker returns a random per-exec marker value.
func newExecMarker() string {
	b := make([]byte, 16)
	// As of Go 1.24 crypto/rand.Read never fails; it aborts the program if the
	// OS source is unavailable.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// killExec best-effort terminates a timed-out exec process (and its
// descendants) inside the container: it scans /proc/*/environ for the exec's
// marker and SIGKILLs every match, plus each match's process group. The
// environ snapshot is fixed at execve time, so a process cannot hide from the
// scan by unsetting the variable; only a re-exec with a scrubbed environment
// escapes, for which the container's pids/memory limits remain the backstop.
func (s *Sandbox) killExec(ctx context.Context, containerID, marker string) {
	script := fmt.Sprintf(
		`for d in /proc/[0-9]*; do if tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qxF %s; then p=${d#/proc/}; kill -9 -"$p" 2>/dev/null; kill -9 "$p" 2>/dev/null; fi; done`,
		sandbox.ShellQuote(execMarkerEnv+"="+marker))
	kill, err := s.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{Cmd: []string{"sh", "-c", script}})
	if err != nil {
		return
	}
	_, _ = s.cli.ExecStart(ctx, kill.ID, client.ExecStartOptions{Detach: true})
}

// Close implements sandbox.Sandbox. In persistent mode it also removes the
// long-lived container.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	id := s.containerID
	s.containerID = ""
	s.mu.Unlock()
	if id != "" {
		if s.opts.Persistent && s.opts.KeepOnClose {
			timeout := 10
			_, _ = s.cli.ContainerStop(context.Background(), id, client.ContainerStopOptions{Timeout: &timeout})
		} else {
			_, _ = s.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		}
	}
	err := s.cli.Close()
	if s.sshDial != nil {
		_ = s.sshDial.Close()
	}
	return err
}

// containerEnv is the environment an EPHEMERAL container is created with:
// the sandbox's own, overridden per entry by the request's — that mode has no
// docker exec to carry them.
func (s *Sandbox) containerEnv(req map[string]string) []string {
	if len(s.opts.Env) == 0 {
		return envSlice(req)
	}
	merged := make(map[string]string, len(s.opts.Env)+len(req))
	maps.Copy(merged, s.opts.Env)
	maps.Copy(merged, req)
	return envSlice(merged)
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Sorted: the container config and the adoption fingerprint must not
	// change with map iteration order.
	slices.Sort(out)
	return out
}

var _ sandbox.Sandbox = (*Sandbox)(nil)
var _ sandbox.ExecStreamer = (*Sandbox)(nil)
