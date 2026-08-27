package docker

import (
	"context"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

// The docker backend's optional capabilities. All three need the long-lived
// container of Persistent mode: an ephemeral sandbox has nothing between
// commands to start, stop or copy out of.

var (
	_ sandbox.Lifecycle = (*Sandbox)(nil)
	_ sandbox.Exporter  = (*Sandbox)(nil)
)

// Start creates the container if it is absent and starts it if it is stopped —
// the same path the first command takes, run on request instead.
func (s *Sandbox) Start(ctx context.Context) error {
	if err := s.requirePersistent(); err != nil {
		return err
	}
	if err := s.ensureImage(ctx); err != nil {
		return err
	}
	_, err := s.ensureContainer(ctx)
	return err
}

// Stop stops the container, keeping it and its volume: installed packages and
// the working tree both survive, and the next command starts it again. It does
// NOT close the sandbox — the daemon connection stays open for Status and the
// file operations.
func (s *Sandbox) Stop(ctx context.Context) error {
	if err := s.requirePersistent(); err != nil {
		return err
	}
	timeout := 10
	if _, err := s.cli.ContainerStop(ctx, s.opts.ContainerName, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil // nothing provisioned: already as stopped as it gets
		}
		return fmt.Errorf("docker sandbox: stopping %s: %w", s.opts.ContainerName, err)
	}
	// The cached id refers to a container that is no longer running; forget it
	// so the next command re-adopts rather than exec'ing into a stopped one.
	s.mu.Lock()
	s.containerID = ""
	s.mu.Unlock()
	return nil
}

// Status inspects the container BY NAME, not by the cached id: a sandbox that
// has never run a command holds no id, and "never used" must read as absent
// rather than as an error.
func (s *Sandbox) Status(ctx context.Context) (sandbox.State, error) {
	if err := s.requirePersistent(); err != nil {
		return sandbox.StateAbsent, err
	}
	info, err := s.cli.ContainerInspect(ctx, s.opts.ContainerName, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return sandbox.StateAbsent, nil
		}
		return sandbox.StateAbsent, fmt.Errorf("docker sandbox: inspecting %s: %w", s.opts.ContainerName, err)
	}
	if info.Container.State != nil && info.Container.State.Running {
		return sandbox.StateRunning, nil
	}
	return sandbox.StateStopped, nil
}

// ExportTar streams path (empty = the whole working directory) out of the
// container as a tar archive. It goes through the same CopyFromContainer the
// file tools use, so it sees exactly what a command sees — and it starts the
// container when it is not running, because the daemon's copy needs one.
func (s *Sandbox) ExportTar(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := s.requirePersistent(); err != nil {
		return nil, err
	}
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	id, err := s.ensureContainer(ctx)
	if err != nil {
		return nil, err
	}
	src := s.containerWorkDir()
	if path != "" {
		src = s.containerPath(path)
	}
	result, err := s.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: src})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: export %s: %w", src, err)
	}
	return result.Content, nil
}

// requirePersistent refuses the capabilities that need a long-lived container.
func (s *Sandbox) requirePersistent() error {
	if !s.opts.Persistent || s.opts.ContainerName == "" {
		return fmt.Errorf("docker sandbox: %w (needs Persistent mode and a ContainerName)", sandbox.ErrLifecycleUnsupported)
	}
	return nil
}

// StartManaged starts the named container on the daemon opts describes,
// refusing one this package did not create. The stop/remove siblings live in
// containers.go; this one completes the trio an operator needs.
func StartManaged(ctx context.Context, opts Options, name string) error {
	return withManaged(ctx, opts, name, func(cli *client.Client, id string) error {
		_, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
		return err
	})
}
