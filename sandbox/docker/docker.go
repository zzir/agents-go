// Package docker implements the sandbox.Sandbox interface using Docker
// containers. Two modes are supported:
//
//   - Ephemeral (default): each Exec creates a fresh container, runs the
//     command, captures output and removes the container.
//   - Persistent (Options.Persistent = true): a single long-lived container is
//     started on the first Exec; subsequent Exec calls use "docker exec" to run
//     commands inside it. The container is removed on Close.
//
// This package pulls the (heavy) Docker client; it is a separate module so the
// core agents-go module stays dependency-light.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
			s.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
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

	tarball, terr := buildTar(nil)
	if terr != nil {
		s.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return "", terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: workDir, Content: tarball}); err != nil {
		s.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return "", fmt.Errorf("docker sandbox: copy files: %w", err)
	}

	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		s.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
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

	execOpts := client.ExecCreateOptions{
		Cmd:          req.Cmd,
		WorkingDir:   workDir,
		Env:          envSlice(req.Env),
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

	max := req.EffectiveMaxOutputBytes()
	stdout, stderr, derr := demuxLogs(attached.Reader, max)

	res := &sandbox.ExecResult{}
	if ectx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Stdout, res.Stderr = stdout, stderr
		// Kill the exec process so it doesn't linger in the container.
		s.killExec(ctx, id, created.ID)
		return res, nil
	}
	if derr != nil {
		return nil, fmt.Errorf("docker sandbox: exec read: %w", derr)
	}

	inspect, ierr := s.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if ierr != nil {
		return nil, fmt.Errorf("docker sandbox: exec inspect: %w", ierr)
	}
	res.ExitCode = inspect.ExitCode
	res.Stdout, res.Stderr = stdout, stderr
	return res, nil
}

func (s *Sandbox) execEphemeral(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	cfg, hostCfg := s.buildConfig(req)
	created, err := s.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID
	defer s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})

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
// read-only root filesystem so that package installs work.
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
// When no explicit User is set and UserUnset is false, the image's default
// user is used (typically a non-root user that can install packages).
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
func (s *Sandbox) readLogs(ctx context.Context, id string, max int64) (string, string, error) {
	rc, err := s.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", "", fmt.Errorf("docker sandbox: logs: %w", err)
	}
	defer rc.Close()
	stdout, stderr, derr := demuxLogs(rc, max)
	if derr != nil {
		return "", "", fmt.Errorf("docker sandbox: read logs: %w", derr)
	}
	return stdout, stderr, nil
}

// demuxLogs splits a multiplexed docker log stream into stdout and stderr,
// capping each at max bytes. Reading stops at the source only once BOTH
// streams are full — a per-total limit would let a flooding stdout starve a
// short stderr (or vice versa) that arrives later in the stream. Memory stays
// bounded throughout because each buffer discards beyond its cap.
func demuxLogs(r io.Reader, max int64) (string, string, error) {
	stdout := &sandbox.CappedBuffer{Max: max}
	stderr := &sandbox.CappedBuffer{Max: max}
	src := &stopWhenFull{r: r, full: func() bool { return stdout.Full() && stderr.Full() }}
	// A cut mid-frame surfaces as ErrUnexpectedEOF, which just means "truncated".
	if _, err := stdcopy.StdCopy(stdout, stderr, src); err != nil &&
		err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", "", err
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

// killExec finds the PID of a running exec process and kills its entire
// process group inside the container so child processes don't linger.
func (s *Sandbox) killExec(ctx context.Context, containerID, execID string) {
	info, err := s.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil || !info.Running || info.PID == 0 {
		return
	}
	// Kill the entire process group (-PID), falling back to the single PID.
	killCmd := client.ExecCreateOptions{
		Cmd: []string{"sh", "-c", fmt.Sprintf("kill -9 -%d 2>/dev/null; kill -9 %d 2>/dev/null", info.PID, info.PID)},
	}
	kill, err := s.cli.ExecCreate(ctx, containerID, killCmd)
	if err != nil {
		return
	}
	s.cli.ExecStart(ctx, kill.ID, client.ExecStartOptions{Detach: true})
}

// shellQuote returns s as a single-quoted shell token, safe for embedding in
// sh -c commands.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReadFile implements sandbox.Sandbox.
func (s *Sandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if s.opts.WorkDir != "" {
		return os.ReadFile(filepath.Join(s.opts.WorkDir, filepath.Clean("/"+p)))
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
		return io.ReadAll(tr)
	}
	return nil, sandbox.ErrNoWorkDir
}

// WriteFile implements sandbox.Sandbox.
func (s *Sandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	if s.opts.WorkDir != "" {
		hostPath := filepath.Join(s.opts.WorkDir, filepath.Clean("/"+p))
		if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(hostPath, content, 0o644)
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

// ListDir implements sandbox.Sandbox.
func (s *Sandbox) ListDir(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	if s.opts.WorkDir != "" {
		hostPath := filepath.Join(s.opts.WorkDir, filepath.Clean("/"+p))
		entries, err := os.ReadDir(hostPath)
		if err != nil {
			return nil, err
		}
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
		cmd := fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -printf '%%y\\t%%s\\t%%f\\n'", shellQuote(dir))
		res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", cmd}})
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("docker sandbox: list dir: %s", res.Stderr)
		}
		lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
		var out []sandbox.DirEntry
		for _, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			out = append(out, sandbox.DirEntry{
				Name:  parts[2],
				IsDir: parts[0] == "d",
				Size:  size,
			})
		}
		return out, nil
	}
	return nil, sandbox.ErrNoWorkDir
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

	execOpts := client.ExecCreateOptions{
		Cmd:          req.Cmd,
		WorkingDir:   workDir,
		Env:          envSlice(req.Env),
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

	if _, err := stdcopy.StdCopy(stdout, stderr, attached.Reader); err != nil &&
		err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
		// ignore
	}

	res := &sandbox.ExecResult{}
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		s.killExec(ctx, id, created.ID)
		return res, nil
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
	defer s.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})

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

	if _, err := stdcopy.StdCopy(stdout, stderr, rc); err != nil &&
		err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
		// ignore
	}

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
		s.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
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
