package sandboxes

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	e2bsb "github.com/zzir/agents-go/sandbox/e2b"
)

// e2bBackend runs a project on any service that speaks the E2B API — E2B's own
// cloud, a self-hosted one, or a compatible service (decisions §5.34). One
// remote sandbox per project, remembered by `projects.instance_ref` so a
// restart resumes it instead of provisioning a second.
type e2bBackend struct{}

func (e2bBackend) Open(spec Spec) (sandbox.Sandbox, error) {
	opts, err := e2bOptions(spec)
	if err != nil {
		return nil, err
	}
	opts.OnSandboxID = spec.SaveInstanceRef
	return e2bsb.New(opts)
}

// Reclaim kills the sandbox, which on these services destroys the filesystem
// with it: the sandbox IS the storage. There is nothing else to remove.
func (e2bBackend) Reclaim(ctx context.Context, spec Spec) error {
	if spec.Project.InstanceRef == "" {
		return nil // nothing was ever provisioned
	}
	opts, err := e2bOptions(spec)
	if err != nil {
		return err
	}
	sb, err := e2bsb.New(opts)
	if err != nil {
		return err
	}
	return sb.Destroy(ctx)
}

// e2bOptions assembles the SDK options from the sandbox and the project.
func e2bOptions(spec Spec) (e2bsb.Options, error) {
	var c store.E2BConfig
	if err := unmarshalConfig(spec.Sandbox.Config, &c); err != nil {
		return e2bsb.Options{}, fmt.Errorf("e2b sandbox: invalid config: %w", err)
	}
	if c.APIKey == "" {
		return e2bsb.Options{}, fmt.Errorf("e2b sandbox %q requires an api_key", spec.Sandbox.Name)
	}
	if c.TemplateID == "" {
		return e2bsb.Options{}, fmt.Errorf("e2b sandbox %q names no template_id", spec.Sandbox.Name)
	}
	env, err := store.EnvMap(spec.Project.Env)
	if err != nil {
		return e2bsb.Options{}, fmt.Errorf("project %s: %w", spec.Project.Name, err)
	}
	return e2bsb.Options{
		APIURL:           c.APIURL,
		Domain:           c.Domain,
		APIKey:           c.APIKey,
		DataPlaneAuth:    e2bsb.DataPlaneAuth(c.DataPlaneAuth),
		TemplateID:       c.TemplateID,
		User:             c.User,
		SandboxID:        spec.Project.InstanceRef,
		TimeoutSeconds:   c.TimeoutSeconds,
		AutoPause:        c.AutoPause,
		AllowInternet:    c.AllowInternet,
		MaxReadFileBytes: c.MaxReadFileBytes,
		Env:              env,
		// The tag an operator needs to tell whose sandbox is whose on the
		// service's own console.
		Metadata: map[string]string{"agents_project": spec.Project.ID, "agents_owner": spec.Project.OwnerID},
	}, nil
}

// Rebuild is refused: on these services the sandbox IS the storage, so
// throwing the compute away throws the working tree away with it. That is
// what Reclaim means, and it is not what a person clicking "rebuild" is
// asking for — they want their files back on a fresh container.
func (e2bBackend) Rebuild(context.Context, Spec) error {
	return fmt.Errorf("this sandbox runs on an E2B-compatible service, where the sandbox IS the storage: " +
		"replacing it would destroy the working tree. Export the project first, then create a new one")
}

// Check provisions a sandbox, runs the health command in it and destroys it
// again — the only way to prove a remote service reachable and its template
// runnable.
func (e2bBackend) Check(ctx context.Context, sb *store.Sandbox) error {
	// A synthetic project: the check needs a sandbox, not a tree, and nothing
	// it provisions outlives the call.
	spec := Spec{Sandbox: sb, Project: &store.Project{ID: "health-check", Name: "health-check"}}
	opts, err := e2bOptions(spec)
	if err != nil {
		return err
	}
	// No callback: this sandbox is destroyed below, and recording it would
	// write a handle onto a project that does not exist.
	opts.OnSandboxID = nil
	opts.Metadata = map[string]string{"agents_health_check": "1"}
	inst, err := e2bsb.New(opts)
	if err != nil {
		return err
	}
	defer func() {
		// WithoutCancel: a cancelled request must not leave a billed sandbox.
		// Bounded, so a hung control plane cannot wedge the check forever.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer cancel()
		if derr := inst.Destroy(dctx); derr != nil {
			logging.Ctx(ctx).Warn("destroying the health-check sandbox", "error", derr)
		}
	}()
	return checkExec(ctx, inst)
}
