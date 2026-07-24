// Package docker implements the sandbox.Sandbox interface using Docker
// containers. Two modes are supported:
//
// - Ephemeral (default): each Exec creates a fresh container, runs the
// command, captures output and removes the container.
// - Persistent (Options.Persistent = true): a single long-lived container is
// started on the first Exec; subsequent Exec calls use "docker exec" to run
// commands inside it. The container is removed on Close.
//
// This package pulls the (heavy) Docker client; it is a separate module so the
// core agents-go module stays dependency-light.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
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
)

// Options configures the Docker sandbox.
type Options struct {
	// Image is the container image to run. Required.
	Image string
	// Host is the Docker daemon address (e.g. "tcp://remote:2375"). When empty,
	// the standard DOCKER_HOST environment variable (or the platform default
	// socket) is used.
	Host string
	// Runtime is the OCI runtime to use (e.g. "runsc" for gVisor). When empty,
	// the daemon's default runtime (usually runc) is used.
	Runtime string
	// Limits caps the container's resources. Zero fields use the defaults below.
	Limits sandbox.Limits
	// User runs the process as the given user[:group]. Defaults to "65534:65534"
	// (nobody). Set to "" via UserUnset to keep the image default.
	User string
	// UserUnset, when true, leaves the image's default user (overrides User).
	UserUnset bool
	// Network, when true, leaves networking enabled. Default false (no network).
	Network bool
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
	// MaxReadFileBytes caps how many bytes ReadFile returns; larger files fail
	// with sandbox.ErrReadLimitExceeded instead of being loaded into host
	// memory. Zero (or negative) means sandbox.DefaultMaxReadFileBytes.
	MaxReadFileBytes int64
}

// Sandbox is a Docker-backed sandbox.Sandbox.
type Sandbox struct {
	cli  *client.Client
	opts Options

	// image pull state
	pullMu   sync.Mutex
	pullDone bool

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
	// API-version negotiation is on by default in the moby client.
	clientOpts := []client.Opt{client.FromEnv}
	if opts.Host != "" {
		clientOpts = append(clientOpts, client.WithHost(opts.Host))
	}
	cli, err := client.New(clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: %w", err)
	}
	if opts.User == "" && !opts.UserUnset {
		opts.User = "65534:65534"
	}
	return &Sandbox{cli: cli, opts: opts}, nil
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

// Exec implements sandbox.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	if req.Stdin != "" {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Stdin is not supported")
	}
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Cmd is empty")
	}
	if s.opts.Persistent {
		return s.execPersistent(ctx, req)
	}
	return s.execEphemeral(ctx, req)
}

// ensureContainer lazily creates and starts the persistent container, or
// recreates it if the existing one has exited (e.g. OOM-killed).
func (s *Sandbox) ensureContainer(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.containerID != "" {
		id := s.containerID
		s.mu.Unlock()
		info, err := s.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err == nil && info.Container.State != nil && info.Container.State.Running {
			return id, nil
		}
		s.mu.Lock()
		if s.containerID == id {
			// WithoutCancel: a canceled ctx would skip the remove while the ID
			// is dropped below, leaking the dead container forever.
			_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			s.containerID = ""
		}
	}
	defer s.mu.Unlock()

	if s.containerID != "" {
		return s.containerID, nil
	}
	return s.createContainer(ctx)
}

func (s *Sandbox) createContainer(ctx context.Context) (string, error) {
	cfg, hostCfg := s.buildPersistentConfig()
	createOpts := client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, Name: s.opts.ContainerName}
	created, err := s.cli.ContainerCreate(ctx, createOpts)
	if err != nil {
		return "", fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID

	// Cleanup must not use ctx directly: when the failure was caused by ctx
	// being canceled (or timing out), a ctx-bound remove would fail too and
	// leak the container.
	rmCtx := context.WithoutCancel(ctx)
	tarball, terr := buildTar(nil)
	if terr != nil {
		_, _ = s.cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return "", terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		_, _ = s.cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return "", fmt.Errorf("docker sandbox: copy files: %w", err)
	}

	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		_, _ = s.cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return "", fmt.Errorf("docker sandbox: start: %w", err)
	}
	s.containerID = id
	return id, nil
}

func (s *Sandbox) execPersistent(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
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
	marker, err := newExecMarker()
	if err != nil {
		return nil, err
	}
	execOpts := client.ExecCreateOptions{
		Cmd:          req.Cmd,
		WorkingDir:   workDir,
		Env:          append(envSlice(req.Env), execMarkerEnv+"="+marker),
		AttachStdout: true,
		AttachStderr: true,
	}
	created, err := s.cli.ExecCreate(ctx, id, execOpts)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec create: %w", err)
	}

	timeout := req.EffectiveTimeout()
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	attached, err := s.cli.ExecAttach(ectx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec attach: %w", err)
	}
	defer attached.Close()

	// The attach connection is a hijacked raw net.Conn: ectx only bounded the
	// handshake above and does NOT interrupt reads on it. Demultiplex in a
	// goroutine and, when the deadline fires, force-close the connection so
	// the blocked read unblocks — otherwise a command that never exits (e.g.
	// "sleep infinity") would hang this call forever.
	maxOut := req.EffectiveMaxOutputBytes()
	type demuxed struct {
		stdout, stderr string
		err            error
	}
	demuxCh := make(chan demuxed, 1)
	go func() {
		o, e, derr := demuxLogs(attached.Reader, maxOut)
		demuxCh <- demuxed{stdout: o, stderr: e, err: derr}
	}()

	var d demuxed
	select {
	case d = <-demuxCh:
	case <-ectx.Done():
		attached.Close() // unblock the raw-conn read
		d = <-demuxCh    // wait for the demuxer so the buffers are quiescent
	}

	res := &sandbox.ExecResult{}
	if ectx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Stdout, res.Stderr = d.stdout, d.stderr
		// Kill the exec process so it doesn't linger in the container.
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return res, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		// The caller's context was canceled; clean up the exec process too.
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return nil, cerr
	}
	if d.err != nil {
		return nil, fmt.Errorf("docker sandbox: exec read: %w", d.err)
	}

	inspect, ierr := s.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if ierr != nil {
		return nil, fmt.Errorf("docker sandbox: exec inspect: %w", ierr)
	}
	res.ExitCode = inspect.ExitCode
	res.Stdout, res.Stderr = d.stdout, d.stderr
	return res, nil
}

func (s *Sandbox) execEphemeral(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	cfg, hostCfg := s.buildConfig(req)
	created, err := s.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID
	defer func() {
		_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()

	// Always sent, even with no request files: the tarball also creates the
	// writable working directory inside the root-owned volume.
	tarball, terr := buildTar(req.Files)
	if terr != nil {
		return nil, terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		return nil, fmt.Errorf("docker sandbox: copy files: %w", err)
	}

	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("docker sandbox: start: %w", err)
	}

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

// buildHostConfig returns the HostConfig. Persistent mode relaxes the
// read-only root filesystem; note that the process still runs as 65534:65534
// by default (see New), so installing packages additionally requires
// UserUnset: true (image default user) or an explicit privileged-enough User.
func (s *Sandbox) buildHostConfig(persistent bool) *container.HostConfig {
	netMode := container.NetworkMode("none")
	if s.opts.Network {
		netMode = container.NetworkMode("default")
	}
	var mounts []mount.Mount
	if s.opts.WorkDir != "" {
		mounts = []mount.Mount{{Type: mount.TypeBind, Source: s.opts.WorkDir, Target: workDir}}
	} else {
		mounts = []mount.Mount{{Type: mount.TypeVolume, Target: workDir}}
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    netMode,
		Runtime:        s.opts.Runtime,
		ReadonlyRootfs: !persistent,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Mounts:         mounts,
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,size=64m,mode=1777"},
		// Cap the container log on disk: without this, a flooding command
		// ("yes" and friends) can write gigabytes into the daemon's log
		// directory within a single timeout window.
		LogConfig: container.LogConfig{
			Type:   "json-file",
			Config: map[string]string{"max-size": logMaxSize, "max-file": "1"},
		},
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
	return hostCfg
}

// buildConfig assembles the container and host configuration for ephemeral mode.
func (s *Sandbox) buildConfig(req sandbox.ExecRequest) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.opts.Image,
		Entrypoint: req.Cmd,
		WorkingDir: workDir,
		Env:        envSlice(req.Env),
		Tty:        false,
	}
	if !s.opts.UserUnset {
		cfg.User = s.opts.User
	}
	return cfg, s.buildHostConfig(false)
}

// buildPersistentConfig assembles the container and host configuration for
// persistent mode. The container runs "sleep infinity" so it stays alive.
// The process runs as 65534:65534 (nobody) by default — New applies that
// default when Options.User is empty and UserUnset is false — which cannot
// install packages even though the root filesystem is writable in this mode.
// To install packages, set UserUnset: true (image default user) or an
// explicit User with sufficient permissions.
func (s *Sandbox) buildPersistentConfig() (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image:      s.opts.Image,
		Entrypoint: []string{"sleep", "infinity"},
		WorkingDir: workDir,
		Tty:        false,
	}
	if !s.opts.UserUnset && s.opts.User != "" {
		cfg.User = s.opts.User
	}
	return cfg, s.buildHostConfig(true)
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
// read and discarded until the other stream also fills or the source ends
// (or, for exec attaches, the timeout severs the connection). Memory stays
// bounded throughout because each buffer discards beyond its cap.
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
func newExecMarker() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("docker sandbox: exec marker: %w", err)
	}
	return hex.EncodeToString(b), nil
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
		shellQuote(execMarkerEnv+"="+marker))
	kill, err := s.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{Cmd: []string{"sh", "-c", script}})
	if err != nil {
		return
	}
	_, _ = s.cli.ExecStart(ctx, kill.ID, client.ExecStartOptions{Detach: true})
}

// shellQuote returns s as a single-quoted shell token, safe for embedding in
// sh -c commands.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReadFile implements sandbox.Sandbox. Files larger than
// Options.MaxReadFileBytes (default sandbox.DefaultMaxReadFileBytes) fail
// with sandbox.ErrReadLimitExceeded instead of being loaded into host memory.
func (s *Sandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		f, err := root.Open(rootRel(p))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return sandbox.ReadAllLimited(f, s.opts.MaxReadFileBytes)
	}
	if s.opts.Persistent {
		if err := s.ensureImage(ctx); err != nil {
			return nil, err
		}
		id, err := s.ensureContainer(ctx)
		if err != nil {
			return nil, err
		}
		fullPath := path.Join(workDir, path.Clean("/"+p))
		result, err := s.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{
			SourcePath: fullPath,
		})
		if err != nil {
			return nil, fmt.Errorf("docker sandbox: read file: %w", err)
		}
		defer result.Content.Close()
		tr := tar.NewReader(result.Content)
		if _, err := tr.Next(); err != nil {
			return nil, fmt.Errorf("docker sandbox: read file: %w", err)
		}
		return sandbox.ReadAllLimited(tr, s.opts.MaxReadFileBytes)
	}
	return nil, sandbox.ErrNoWorkDir
}

// WriteFile implements sandbox.Sandbox.
func (s *Sandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return err
		}
		defer root.Close()
		rel := rootRel(p)
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		return root.WriteFile(rel, content, 0o644)
	}
	if s.opts.Persistent {
		if err := s.ensureImage(ctx); err != nil {
			return err
		}
		id, err := s.ensureContainer(ctx)
		if err != nil {
			return err
		}
		cleanPath := path.Clean("/" + p)[1:]
		tarball, terr := buildTar(map[string]string{cleanPath: string(content)})
		if terr != nil {
			return terr
		}
		if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
			return fmt.Errorf("docker sandbox: write file: %w", err)
		}
		return nil
	}
	return sandbox.ErrNoWorkDir
}

// CreateExclusive implements sandbox.Sandbox atomically: bind-mount mode uses
// O_EXCL under os.Root; persistent mode uses the shell's noclobber (set -C),
// which makes the redirect fail if the target exists. The parent is created
// first so adding into a new directory works.
func (s *Sandbox) CreateExclusive(ctx context.Context, p string, content []byte) error {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return err
		}
		defer root.Close()
		rel := rootRel(p)
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, werr := f.Write(content); werr != nil {
			_ = f.Close()
			return werr
		}
		return f.Close()
	}
	if s.opts.Persistent {
		if err := s.ensureImage(ctx); err != nil {
			return err
		}
		cleanPath := path.Clean("/" + p)[1:]
		if cleanPath == "" {
			return fmt.Errorf("docker sandbox: invalid file path %q", p)
		}
		b64 := base64.StdEncoding.EncodeToString(content)
		script := "set -C && "
		if parent := path.Dir(cleanPath); parent != "." && parent != "" {
			script = "mkdir -p " + shellQuote(parent) + " && set -C && "
		}
		script += "printf %s " + shellQuote(b64) + " | base64 -d > " + shellQuote(cleanPath)
		res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", script}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			if strings.Contains(res.Stderr, "cannot overwrite") || strings.Contains(res.Stderr, "exists") {
				return fmt.Errorf("docker sandbox: create %q: %w", p, fs.ErrExist)
			}
			return fmt.Errorf("docker sandbox: create %q: %s", p, res.Stderr)
		}
		return nil
	}
	return sandbox.ErrNoWorkDir
}

// RemoveFile implements sandbox.Sandbox.
func (s *Sandbox) RemoveFile(ctx context.Context, p string) error {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return err
		}
		defer root.Close()
		return root.Remove(rootRel(p))
	}
	if s.opts.Persistent {
		clean := path.Clean("/" + p)[1:]
		if clean == "" {
			return fmt.Errorf("docker sandbox: invalid file path %q", p)
		}
		res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"rm", "--", clean}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("docker sandbox: rm %q: %s", p, res.Stderr)
		}
		return nil
	}
	return sandbox.ErrNoWorkDir
}

// Rename implements sandbox.Sandbox.
func (s *Sandbox) Rename(ctx context.Context, oldPath, newPath string) error {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return err
		}
		defer root.Close()
		to := rootRel(newPath)
		if dir := filepath.Dir(to); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		return root.Rename(rootRel(oldPath), to)
	}
	if s.opts.Persistent {
		oc := path.Clean("/" + oldPath)[1:]
		nc := path.Clean("/" + newPath)[1:]
		if oc == "" || nc == "" {
			return fmt.Errorf("docker sandbox: invalid rename %q -> %q", oldPath, newPath)
		}
		if parent := path.Dir(nc); parent != "." {
			if res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"mkdir", "-p", "--", parent}}); err != nil {
				return err
			} else if res.ExitCode != 0 {
				return fmt.Errorf("docker sandbox: mkdir %s: %s", parent, res.Stderr)
			}
		}
		res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"mv", "--", oc, nc}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("docker sandbox: mv %q -> %q: %s", oldPath, newPath, res.Stderr)
		}
		return nil
	}
	return sandbox.ErrNoWorkDir
}

// ListDir implements sandbox.Sandbox.
func (s *Sandbox) ListDir(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	if s.opts.WorkDir != "" {
		root, err := os.OpenRoot(s.opts.WorkDir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		f, err := root.Open(rootRel(p))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.ReadDir(-1)
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		out := make([]sandbox.DirEntry, 0, len(entries))
		for _, e := range entries {
			info, ierr := e.Info()
			var size int64
			if ierr == nil {
				size = info.Size()
			}
			out = append(out, sandbox.DirEntry{
				Name:  e.Name(),
				IsDir: e.IsDir(),
				Size:  size,
			})
		}
		return out, nil
	}
	if s.opts.Persistent {
		if err := s.ensureImage(ctx); err != nil {
			return nil, err
		}
		dir := path.Join(workDir, path.Clean("/"+p))
		// NUL-terminate each record instead of newline: a filename may contain
		// a newline (or a tab), which would otherwise split into a phantom
		// entry or corrupt the next line. The name is the final \t-field, so a
		// tab inside it is preserved by the 3-way split; NUL can never appear
		// in a filename, so records stay unambiguous.
		cmd := fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -printf '%%y\\t%%s\\t%%f\\0'", shellQuote(dir))
		res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", cmd}})
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			// A missing directory must surface as fs.ErrNotExist so callers can
			// tell "absent" from a real failure, uniform with the bind-mount and
			// local/ssh backends.
			if strings.Contains(res.Stderr, "No such file") {
				return nil, fmt.Errorf("docker sandbox: list dir %q: %w", p, fs.ErrNotExist)
			}
			return nil, fmt.Errorf("docker sandbox: list dir: %s", res.Stderr)
		}
		return parseFindEntries(res.Stdout), nil
	}
	return nil, sandbox.ErrNoWorkDir
}

// rootRel converts a sandbox-relative path — which may carry a leading slash or
// ".." components — into a clean path relative to an os.Root. Cleaning as if
// rooted at "/" neutralizes any ".." that would escape, and the leading
// separator is then stripped because os.Root rejects rooted names. An empty or
// root-only path becomes ".", i.e. the bind-mounted working directory itself.
func rootRel(p string) string {
	rel := strings.TrimPrefix(filepath.Clean("/"+p), string(filepath.Separator))
	if rel == "" {
		return "."
	}
	return rel
}

// parseFindEntries parses the NUL-separated output of the persistent-mode
// ListDir "find" command. Each record is "%y\t%s\t%f" — type char, size,
// filename — and records are separated by NUL so a filename containing a tab or
// newline cannot corrupt the listing. A trailing NUL yields an empty final
// record, which is skipped.
func parseFindEntries(out string) []sandbox.DirEntry {
	records := strings.Split(out, "\x00")
	entries := make([]sandbox.DirEntry, 0, len(records))
	for _, rec := range records {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		entries = append(entries, sandbox.DirEntry{
			Name:  parts[2],
			IsDir: parts[0] == "d",
			Size:  size,
		})
	}
	return entries
}

// ExecStream implements sandbox.ExecStreamer.
func (s *Sandbox) ExecStream(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	if req.Stdin != "" {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Stdin is not supported")
	}
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Cmd is empty")
	}
	if s.opts.Persistent {
		return s.streamPersistent(ctx, req, stdout, stderr)
	}
	return s.streamEphemeral(ctx, req, stdout, stderr)
}

func (s *Sandbox) streamPersistent(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
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
	marker, err := newExecMarker()
	if err != nil {
		return nil, err
	}
	execOpts := client.ExecCreateOptions{
		Cmd:          req.Cmd,
		WorkingDir:   workDir,
		Env:          append(envSlice(req.Env), execMarkerEnv+"="+marker),
		AttachStdout: true,
		AttachStderr: true,
	}
	created, err := s.cli.ExecCreate(ctx, id, execOpts)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec create: %w", err)
	}

	timeout := req.EffectiveTimeout()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	attached, err := s.cli.ExecAttach(tctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec attach: %w", err)
	}
	defer attached.Close()

	// Same as execPersistent: the hijacked raw connection ignores tctx, so
	// copy in a goroutine and force-close the connection on timeout to
	// unblock the read. Copy errors from a severed stream are expected and
	// intentionally dropped (partial output already reached the writers).
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = stdcopy.StdCopy(stdout, stderr, attached.Reader)
	}()

	select {
	case <-copyDone:
	case <-tctx.Done():
		attached.Close() // unblock the raw-conn read
		<-copyDone       // no writes to stdout/stderr after we return
	}

	res := &sandbox.ExecResult{}
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return res, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		// The caller's context was canceled; clean up the exec process too.
		s.killExec(context.WithoutCancel(ctx), id, marker)
		return nil, cerr
	}

	inspect, ierr := s.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if ierr != nil {
		return nil, fmt.Errorf("docker sandbox: exec inspect: %w", ierr)
	}
	res.ExitCode = inspect.ExitCode
	return res, nil
}

func (s *Sandbox) streamEphemeral(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	cfg, hostCfg := s.buildConfig(req)
	created, err := s.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID
	defer func() {
		_, _ = s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()

	tarball, terr := buildTar(req.Files)
	if terr != nil {
		return nil, terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		return nil, fmt.Errorf("docker sandbox: copy files: %w", err)
	}

	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("docker sandbox: start: %w", err)
	}

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

	// Copy errors here only lose already-truncated log bytes; ignore them.
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

// Close implements sandbox.Sandbox. In persistent mode it also removes the
// long-lived container.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	id := s.containerID
	s.containerID = ""
	s.mu.Unlock()
	if id != "" {
		_, _ = s.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}
	return s.cli.Close()
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// buildTar packs files into a tar stream extracted at workDir. Parent
// directories of nested files are created world-writable.
func buildTar(files map[string]string) (io.Reader, error) {
	names := make([]string, 0, len(files))
	clean := make(map[string]string, len(files))
	dirSet := map[string]bool{}
	for name, content := range files {
		cn := path.Clean("/" + name)[1:] // strip leading slash, prevent traversal
		if cn == "" {
			return nil, fmt.Errorf("docker sandbox: invalid file path %q", name)
		}
		if _, dup := clean[cn]; dup {
			return nil, fmt.Errorf("docker sandbox: duplicate file path %q", cn)
		}
		clean[cn] = content
		names = append(names, cn)
		for d := path.Dir(cn); d != "."; d = path.Dir(d) {
			dirSet[d] = true
		}
	}
	sort.Strings(names)
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // parents sort before children

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeDir := func(name string, mode int64) error {
		return tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeDir,
			Mode:     mode,
			ModTime:  time.Unix(0, 0),
		})
	}
	for _, d := range dirs {
		if err := writeDir(d, 0o777); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		content := clean[name]
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

var _ sandbox.Sandbox = (*Sandbox)(nil)
var _ sandbox.ExecStreamer = (*Sandbox)(nil)
