package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ManagedContainer is one container this package created (it carries the
// fingerprint label), as an admin listing shows it.
type ManagedContainer struct {
	Name    string    `json:"name"`
	Image   string    `json:"image"`
	State   string    `json:"state"`  // running | exited | ...
	Status  string    `json:"status"` // human-readable, e.g. "Up 2 hours"
	Created time.Time `json:"created"`
}

// managedClient dials the daemon opts describes — the same connection New
// makes, without a Sandbox around it.
func managedClient(opts Options) (*client.Client, func(), error) {
	clientOpts := []client.Opt{client.FromEnv}
	var sshDial *sshDialer
	switch {
	case strings.HasPrefix(opts.Host, "ssh://"):
		d, err := newSSHDialer(opts.Host, opts.SSH)
		if err != nil {
			return nil, nil, err
		}
		sshDial = d
		clientOpts = append(clientOpts, client.WithHost("http://docker.invalid"), client.WithDialContext(d.DialContext))
	case opts.Host != "":
		clientOpts = append(clientOpts, client.WithHost(opts.Host))
	}
	cli, err := client.New(clientOpts...)
	if err != nil {
		if sshDial != nil {
			_ = sshDial.Close()
		}
		return nil, nil, fmt.Errorf("docker sandbox: %w", err)
	}
	closeAll := func() {
		_ = cli.Close()
		if sshDial != nil {
			_ = sshDial.Close()
		}
	}
	return cli, closeAll, nil
}

// ListManaged lists this package's containers on the daemon opts describes —
// running and stopped alike, matched by the fingerprint label so foreign
// containers never appear. Only Host/SSH of opts are used.
func ListManaged(ctx context.Context, opts Options) ([]ManagedContainer, error) {
	cli, done, err := managedClient(opts)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: listing containers: %w", err)
	}
	var out []ManagedContainer
	for _, c := range res.Items {
		if _, ours := c.Labels[fingerprintLabel]; !ours {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ManagedContainer{
			Name:    name,
			Image:   c.Image,
			State:   string(c.State),
			Status:  c.Status,
			Created: time.Unix(c.Created, 0).UTC(),
		})
	}
	return out, nil
}

// StopManaged stops the named container on the daemon opts describes,
// refusing one this package did not create.
func StopManaged(ctx context.Context, opts Options, name string) error {
	return withManaged(ctx, opts, name, func(cli *client.Client, id string) error {
		timeout := 10
		_, err := cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
		return err
	})
}

// RemoveManaged force-removes the named container — the "rebuild" act: the
// next run recreates it from the current configuration. Refuses a container
// this package did not create.
func RemoveManaged(ctx context.Context, opts Options, name string) error {
	return withManaged(ctx, opts, name, func(cli *client.Client, id string) error {
		_, err := cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
		return err
	})
}

// ManagedNamePrefix is the guard prefix for a volume reclaim: this package does
// not mint names (the caller supplies ContainerName/VolumeName), so a volume —
// which carries no ownership label a container's fingerprint would — is only
// removed when its name begins with this. Callers derive names with it.
const ManagedNamePrefix = "agents-"

// ErrVolumeNotFound reports a volume call naming a volume that is not there.
// A caller reclaiming storage has already got what it asked for and continues.
var ErrVolumeNotFound = errors.New("volume not found")

// RemoveManagedVolume removes the named volume from the daemon opts describes
// — the storage reclaim. Unlike the container calls there is no ownership
// label to verify (the daemon creates a named volume implicitly on first
// mount), so this refuses any name outside ManagedNamePrefix; callers derive
// the name from an id they own and never take it from a request.
func RemoveManagedVolume(ctx context.Context, opts Options, name string) error {
	if !strings.HasPrefix(name, ManagedNamePrefix) {
		return fmt.Errorf("docker sandbox: %q is not a managed volume name", name)
	}
	cli, done, err := managedClient(opts)
	if err != nil {
		return err
	}
	defer done()
	if _, err := cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker sandbox: %s: %w", name, ErrVolumeNotFound)
		}
		return fmt.Errorf("docker sandbox: %w", err)
	}
	return nil
}

// ErrContainerNotFound reports a managed-container call naming a container
// that is not there. A caller removing one to replace it has already got what
// it asked for and continues; a caller acting on a listing reports it.
var ErrContainerNotFound = errors.New("container not found")

// withManaged verifies ownership (the fingerprint label) before act runs, and
// hands act the inspected ID, never the name: IDs are not reused under a race.
func withManaged(ctx context.Context, opts Options, name string, act func(cli *client.Client, id string) error) error {
	cli, done, err := managedClient(opts)
	if err != nil {
		return err
	}
	defer done()
	info, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker sandbox: %s: %w", name, ErrContainerNotFound)
		}
		return fmt.Errorf("docker sandbox: %w", err)
	}
	c := info.Container
	if err := ensureOwned(c.Config, name); err != nil {
		return err
	}
	return act(cli, c.ID)
}

// ensureOwned is the ownership boundary the by-name entry points share: a
// container without this package's fingerprint label is foreign. cfg is nil-safe.
func ensureOwned(cfg *container.Config, name string) error {
	var labels map[string]string
	if cfg != nil {
		labels = cfg.Labels
	}
	if _, ours := labels[fingerprintLabel]; !ours {
		return fmt.Errorf("docker sandbox: container %q was not created by this package", name)
	}
	return nil
}
