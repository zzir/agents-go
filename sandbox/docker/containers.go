package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
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

// dial connects to the daemon opts describes; the returned func releases the
// client and, for an ssh:// host, its tunnel.
func dial(opts Options) (*client.Client, func(), error) {
	// API-version negotiation is on by default in the moby client.
	clientOpts := []client.Opt{client.FromEnv}
	var sshDial *sshDialer
	switch {
	case strings.HasPrefix(opts.Host, "ssh://"):
		d, err := newSSHDialer(opts.Host, opts.SSH)
		if err != nil {
			return nil, nil, err
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
		return nil, nil, fmt.Errorf("docker sandbox: %w", err)
	}
	return cli, func() {
		_ = cli.Close()
		if sshDial != nil {
			_ = sshDial.Close()
		}
	}, nil
}

// stopTimeoutSeconds is the grace a container gets between SIGTERM and SIGKILL.
const stopTimeoutSeconds = 10

// stopContainer stops a container by id, keeping it and its volumes.
func stopContainer(ctx context.Context, cli *client.Client, id string) error {
	timeout := stopTimeoutSeconds
	_, err := cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

// ListManaged lists this package's containers on the daemon opts describes —
// running and stopped alike, matched by the fingerprint label so foreign
// containers never appear. Only Host/SSH of opts are used.
func ListManaged(ctx context.Context, opts Options) ([]ManagedContainer, error) {
	cli, done, err := dial(opts)
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
		return stopContainer(ctx, cli, id)
	})
}

// RemoveManaged force-removes the named container, so the next run recreates
// it from the current configuration; refuses one this package did not create.
func RemoveManaged(ctx context.Context, opts Options, name string) error {
	return withManaged(ctx, opts, name, func(cli *client.Client, id string) error {
		_, err := cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
		return err
	})
}

// ManagedNamePrefix guards a volume reclaim: a volume carries no ownership
// label, so RemoveManagedVolume refuses a name outside it. Callers derive names
// with it.
const ManagedNamePrefix = "agents-"

// ErrVolumeNotFound reports a volume call naming a volume that is not there.
var ErrVolumeNotFound = errors.New("volume not found")

// RemoveManagedVolume removes the named volume from the daemon opts describes,
// refusing a name outside ManagedNamePrefix.
func RemoveManagedVolume(ctx context.Context, opts Options, name string) error {
	if !strings.HasPrefix(name, ManagedNamePrefix) {
		return fmt.Errorf("docker sandbox: %q is not a managed volume name", name)
	}
	cli, done, err := dial(opts)
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
// that is not there.
var ErrContainerNotFound = errors.New("container not found")

// withManaged verifies ownership (the fingerprint label) before act runs, and
// hands act the inspected ID, never the name: IDs are not reused under a race.
func withManaged(ctx context.Context, opts Options, name string, act func(cli *client.Client, id string) error) error {
	cli, done, err := dial(opts)
	if err != nil {
		return err
	}
	defer done()
	id, _, found, err := inspectOwned(ctx, cli, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("docker sandbox: %s: %w", name, ErrContainerNotFound)
	}
	return act(cli, id)
}

// inspectOwned is the ownership boundary the by-name entry points share: found
// is false when nothing holds the name, and a holder without this package's
// fingerprint label is FOREIGN, an error.
func inspectOwned(ctx context.Context, cli *client.Client, name string) (id string, running, found bool, err error) {
	info, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf("docker sandbox: inspecting %s: %w", name, err)
	}
	c := info.Container
	var labels map[string]string
	if c.Config != nil {
		labels = c.Config.Labels
	}
	if _, ours := labels[fingerprintLabel]; !ours {
		return "", false, false, fmt.Errorf("docker sandbox: container %q was not created by this package", name)
	}
	return c.ID, c.State != nil && c.State.Running, true, nil
}
