// Package docker implements the sandbox.Sandbox interface using ephemeral Docker
// containers. Each Exec creates a locked-down, network-isolated container, runs
// the command, captures output and removes the container.
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
	"path"
	"sort"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/zzir/agents-go/sandbox"
)

const (
	// volumeDir is where the anonymous volume backing the working directory is
	// mounted. Its root stays owned by root (0755) — the daemon refuses to
	// docker-cp into a read-only root fs anywhere but a volume, and it will not
	// chmod an existing mount point — so the writable working directory is a
	// subdirectory created from the file tarball with mode 1777.
	volumeDir = "/sandbox"
	// workDirName is the working directory relative to volumeDir; tar entries
	// are rooted at it.
	workDirName = "work"
	// workDir is the directory the command runs in. It is world-writable
	// (sticky), so the unprivileged sandbox user can write scratch files.
	workDir = volumeDir + "/" + workDirName
	// logReadTimeout bounds reading the container logs after a timeout, when
	// the request's own context is already spent.
	logReadTimeout = 10 * time.Second
)

// Options configures the Docker sandbox.
type Options struct {
	// Image is the container image to run (must be available locally). Required.
	Image string
	// Limits caps the container's resources. Zero fields use the defaults below.
	Limits sandbox.Limits
	// User runs the process as the given user[:group]. Defaults to "65534:65534"
	// (nobody). Set to "" via UserUnset to keep the image default.
	User string
	// UserUnset, when true, leaves the image's default user (overrides User).
	UserUnset bool
	// Network, when true, leaves networking enabled. Default false (no network).
	Network bool
}

// Sandbox is a Docker-backed sandbox.Sandbox.
type Sandbox struct {
	cli  *client.Client
	opts Options
}

// New connects to the Docker daemon (via the standard environment variables) and
// returns a Sandbox running opts.Image.
func New(opts Options) (*Sandbox, error) {
	if opts.Image == "" {
		return nil, fmt.Errorf("docker sandbox: Image is required")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: %w", err)
	}
	if opts.User == "" && !opts.UserUnset {
		opts.User = "65534:65534"
	}
	return &Sandbox{cli: cli, opts: opts}, nil
}

// Exec implements sandbox.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if req.Stdin != "" {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Stdin is not supported")
	}
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("docker sandbox: ExecRequest.Cmd is empty")
	}

	cfg, hostCfg := s.buildConfig(req)
	created, err := s.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: create: %w", err)
	}
	id := created.ID
	defer s.cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true, RemoveVolumes: true})

	// Always sent, even with no request files: the tarball also creates the
	// writable working directory inside the root-owned volume.
	tarball, terr := buildTar(req.Files)
	if terr != nil {
		return nil, terr
	}
	if err := s.cli.CopyToContainer(ctx, id, volumeDir, tarball, container.CopyToContainerOptions{}); err != nil {
		return nil, fmt.Errorf("docker sandbox: copy files: %w", err)
	}

	if err := s.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("docker sandbox: start: %w", err)
	}

	timeout := req.EffectiveTimeout()
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &sandbox.ExecResult{}
	statusCh, errCh := s.cli.ContainerWait(wctx, id, container.WaitConditionNotRunning)
	select {
	case status := <-statusCh:
		if status.Error != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %s", status.Error.Message)
		}
		res.ExitCode = int(status.StatusCode)
	case werr := <-errCh:
		if cerr := ctx.Err(); cerr != nil {
			// The caller's context was canceled or hit its own deadline; that
			// is not an execution timeout.
			return nil, cerr
		}
		if wctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.ExitCode = -1
			_ = s.cli.ContainerKill(context.WithoutCancel(ctx), id, "KILL")
		} else if werr != nil {
			return nil, fmt.Errorf("docker sandbox: wait: %w", werr)
		}
	}

	// After a timeout wctx is spent; read the logs on a fresh, briefly-bounded
	// context so the timeout result is not turned into a logs error.
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

// buildConfig assembles the container and host configuration with the security
// defaults applied.
func (s *Sandbox) buildConfig(req sandbox.ExecRequest) (*container.Config, *container.HostConfig) {
	cfg := &container.Config{
		Image: s.opts.Image,
		// Entrypoint (not Cmd) carries the full command: with a non-empty
		// entrypoint the daemon inherits neither the image's ENTRYPOINT nor its
		// CMD, so req.Cmd runs exactly as given. Cmd alone would be appended as
		// arguments to an image ENTRYPOINT.
		Entrypoint: req.Cmd,
		WorkingDir: workDir,
		Env:        envSlice(req.Env),
		Tty:        false,
	}
	if !s.opts.UserUnset {
		cfg.User = s.opts.User
	}

	netMode := container.NetworkMode("none")
	if s.opts.Network {
		netMode = container.NetworkMode("default")
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    netMode,
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		// An anonymous volume gives a mount that exists at create time, so the
		// working directory and files can be copied in before start (a tmpfs
		// would not be mounted yet, and the daemon refuses to copy into the
		// read-only root fs). It is removed with the container.
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Target: volumeDir}},
		// A writable (but non-executable) /tmp: with the root fs read-only and
		// no /tmp, many programs would have no scratch space at all.
		Tmpfs: map[string]string{"/tmp": "rw,noexec,size=64m,mode=1777"},
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
	return cfg, hostCfg
}

// readLogs fetches the container output, capping each stream at max bytes.
func (s *Sandbox) readLogs(ctx context.Context, id string, max int64) (string, string, error) {
	rc, err := s.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
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
	stdout := &cappedBuffer{max: max}
	stderr := &cappedBuffer{max: max}
	src := &stopWhenFull{r: r, full: func() bool { return stdout.full() && stderr.full() }}
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

// cappedBuffer is an io.Writer that keeps at most max bytes and silently
// discards the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remain := b.max - int64(b.buf.Len()); remain > 0 {
		if int64(len(p)) > remain {
			b.buf.Write(p[:remain])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// full reports whether the buffer has reached its cap.
func (b *cappedBuffer) full() bool { return int64(b.buf.Len()) >= b.max }

// Close implements sandbox.Sandbox.
func (s *Sandbox) Close() error { return s.cli.Close() }

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

// buildTar packs files into a tar stream rooted at volumeDir. The first entry
// is the working directory itself with mode 1777, making it writable for the
// unprivileged sandbox user even though the surrounding volume root is owned
// by root; parent directories of nested files are created world-writable too.
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
	if err := writeDir(workDirName, 0o1777); err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if err := writeDir(workDirName+"/"+d, 0o777); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		content := clean[name]
		hdr := &tar.Header{
			Name:    workDirName + "/" + name,
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
