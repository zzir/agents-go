package docker

import (
	"context"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

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

// Stop stops the container, keeping it and its volume; the next command starts
// it again. The daemon connection stays open for Status and the file operations.
func (s *Sandbox) Stop(ctx context.Context) error {
	if err := s.requirePersistent(); err != nil {
		return err
	}
	// Ownership first, like every by-name entry point (withManaged), and the stop
	// then acts on the id, not the name — an id is not reused.
	id, _, ok, err := inspectOwned(ctx, s.cli, s.opts.ContainerName)
	if err != nil {
		return err
	}
	if !ok {
		return nil // nothing provisioned: already as stopped as it gets
	}
	if err := stopContainer(ctx, s.cli, id); err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
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

// Status inspects the container BY NAME, so a sandbox that never ran a command
// reads as absent; a foreign holder of the name is an error, never a state.
func (s *Sandbox) Status(ctx context.Context) (sandbox.State, error) {
	if err := s.requirePersistent(); err != nil {
		return sandbox.StateAbsent, err
	}
	_, running, ok, err := inspectOwned(ctx, s.cli, s.opts.ContainerName)
	if err != nil {
		return sandbox.StateAbsent, err
	}
	if !ok {
		return sandbox.StateAbsent, nil
	}
	if running {
		return sandbox.StateRunning, nil
	}
	return sandbox.StateStopped, nil
}

// ExportTar streams the working directory out of the container as a tar
// archive, starting the container first: the daemon's copy needs a running one.
func (s *Sandbox) ExportTar(ctx context.Context) (io.ReadCloser, error) {
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
	result, err := s.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: workDir})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: export %s: %w", workDir, err)
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
