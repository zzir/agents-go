package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// withManaged verifies ownership (the fingerprint label) before act runs —
// these entry points take a NAME, and must never act on a foreign container
// that happens to hold it. act receives the inspected container's ID, never
// the name: IDs are not reused, so a remove+recreate racing the inspect
// cannot hand the name — and the act — to a foreign container.
func withManaged(ctx context.Context, opts Options, name string, act func(cli *client.Client, id string) error) error {
	cli, done, err := managedClient(opts)
	if err != nil {
		return err
	}
	defer done()
	info, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker sandbox: %w", err)
	}
	c := info.Container
	var labels map[string]string
	if c.Config != nil {
		labels = c.Config.Labels
	}
	if _, ours := labels[fingerprintLabel]; !ours {
		return fmt.Errorf("docker sandbox: container %q was not created by this package", name)
	}
	return act(cli, c.ID)
}
