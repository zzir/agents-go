package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The binding-time project rules: the named project must be the owner's, on
// the named sandbox; unnamed resolves to the owner's default project on that
// sandbox, created on first use.
func TestResolveBindingProject(t *testing.T) {
	runner, db := newBareRunner(t)
	ctx := context.Background()
	sandboxes := store.NewSandboxStore(db)
	cfg := &store.SandboxConfig{ID: store.NewID(), Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := sandboxes.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	other := &store.SandboxConfig{ID: store.NewID(), Name: "sb2", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := sandboxes.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	// Unnamed: the default project is created once and found thereafter.
	def, err := runner.resolveBindingProject(ctx, store.LocalUserID, cfg.ID, "")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if def.Name != store.DefaultProjectName || def.SandboxID != cfg.ID {
		t.Fatalf("default project = %+v", def)
	}
	again, err := runner.resolveBindingProject(ctx, store.LocalUserID, cfg.ID, "")
	if err != nil || again.ID != def.ID {
		t.Fatalf("second resolve = %+v, %v; want the same row", again, err)
	}

	// Named: the owner's project on that sandbox resolves.
	named, err := runner.resolveBindingProject(ctx, store.LocalUserID, cfg.ID, def.ID)
	if err != nil || named.ID != def.ID {
		t.Fatalf("named resolve = %+v, %v", named, err)
	}

	var invalid ErrInvalidBinding
	// Unknown project id.
	if _, err := runner.resolveBindingProject(ctx, store.LocalUserID, cfg.ID, store.NewID()); !errors.As(err, &invalid) {
		t.Fatalf("unknown project: %v, want ErrInvalidBinding", err)
	}
	// A foreign owner's project reads as absent.
	foreign := &store.Project{OwnerID: store.NewID(), SandboxID: cfg.ID, Name: "theirs"}
	if err := runner.Deps.Projects.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.resolveBindingProject(ctx, store.LocalUserID, cfg.ID, foreign.ID); !errors.As(err, &invalid) {
		t.Fatalf("foreign project: %v, want ErrInvalidBinding", err)
	}
	// A project on a different sandbox is refused.
	if _, err := runner.resolveBindingProject(ctx, store.LocalUserID, other.ID, def.ID); !errors.As(err, &invalid) {
		t.Fatalf("cross-sandbox project: %v, want ErrInvalidBinding", err)
	}
}
