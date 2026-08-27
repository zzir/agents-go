package sandboxes

import (
	"context"
	"errors"
	"fmt"

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
