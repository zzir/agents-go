package sandboxes

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// dockerBackend is the Docker implementation: one persistent container per
// project over a named volume on the target's daemon (decisions §5.33).
type dockerBackend struct{}

func (dockerBackend) Open(spec Spec) (sandbox.Sandbox, error) {
	opts, err := BuildOptions(spec)
	if err != nil {
		return nil, err
	}
	return dockersb.New(opts)
}

// Rebuild removes the container so the next Open creates a fresh one from the
// current template. The REMOVE is the point: closing an instance only stops
// the container, and a stopped one whose fingerprint still matches is adopted
// again — an evict-only rebuild would hand back exactly what it was asked to
// discard. The volume survives: this replaces the container, not the tree.
func (dockerBackend) Rebuild(ctx context.Context, spec Spec) error {
	opts, err := TargetOptions(spec.Target)
	if err != nil {
		return err
	}
	name := ContainerName(spec.Project.ID)
	if err := dockersb.RemoveManaged(ctx, opts, name); err != nil && !errors.Is(err, dockersb.ErrContainerNotFound) {
		return fmt.Errorf("removing container %s: %w", name, err)
	}
	return nil
}

// Check runs the health command in a throw-away EPHEMERAL container: it needs
// no project tree, and must not leave a persistent container behind.
func (dockerBackend) Check(ctx context.Context, target *store.SandboxTarget, template *store.SandboxTemplate) error {
	opts, err := TargetOptions(target)
	if err != nil {
		return err
	}
	// Borrow the template's image and shape, leave its container, volume and
	// persistence behind.
	full, err := BuildOptions(Spec{Target: target, Template: template, Project: &store.Project{}})
	if err != nil {
		return err
	}
	opts.Image, opts.Runtime, opts.User, opts.Network = full.Image, full.Runtime, full.User, full.Network
	opts.Limits = full.Limits
	sb, err := dockersb.New(opts)
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()
	return checkExec(ctx, sb)
}

// Reclaim removes the container and then the volume. Both "not found" cases
// are success: the caller asked for the storage to be gone.
func (dockerBackend) Reclaim(ctx context.Context, spec Spec) error {
	opts, err := TargetOptions(spec.Target)
	if err != nil {
		return err
	}
	name := ContainerName(spec.Project.ID)
	if err := dockersb.RemoveManaged(ctx, opts, name); err != nil && !errors.Is(err, dockersb.ErrContainerNotFound) {
		return fmt.Errorf("removing container %s: %w", name, err)
	}
	vol := ProjectVolumeName(spec.Project.ID)
	if err := dockersb.RemoveManagedVolume(ctx, opts, vol); err != nil && !errors.Is(err, dockersb.ErrVolumeNotFound) {
		return fmt.Errorf("removing volume %s: %w", vol, err)
	}
	return nil
}
