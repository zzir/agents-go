package bridge

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The binding-time workdir rules: what canonicalizes, what is refused.
// Run-time leniency (effectiveWorkDir) is covered separately — these rules
// gate only NEW bindings.
func TestResolveBindingWorkDir(t *testing.T) {
	cfg := func(typ, config string) *store.SandboxConfig {
		return &store.SandboxConfig{Type: typ, Config: json.RawMessage(config)}
	}
	dockerEphemeral := cfg("docker", `{"image":"i"}`)
	dockerPersistent := cfg("docker", `{"image":"i","persistent":true}`)

	ok := []struct {
		name string
		cfg  *store.SandboxConfig
		in   string
		want string
	}{
		{"ephemeral empty", dockerEphemeral, "", ""},
		{"ephemeral /workspace is the default", dockerEphemeral, "/workspace", ""},
		{"persistent empty", dockerPersistent, "", ""},
		{"/workspace itself is the default", dockerPersistent, "/workspace", ""},
		{"/workspace/ normalizes to the default", dockerPersistent, "/workspace/", ""},
		{"subtree cleans", dockerPersistent, "/workspace//proj/", "/workspace/proj"},
		{"whitespace trims", dockerPersistent, "  /workspace/app  ", "/workspace/app"},
	}
	for _, tc := range ok {
		got, err := ResolveBindingWorkDir(tc.cfg, tc.in)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		} else if got != tc.want {
			t.Errorf("%s: = %q, want %q", tc.name, got, tc.want)
		}
	}

	refused := []struct {
		name string
		cfg  *store.SandboxConfig
		in   string
	}{
		{"ephemeral any dir", dockerEphemeral, "/workspace/x"},
		{"outside /workspace", dockerPersistent, "/tmp/project"},
		{"escape via ..", dockerPersistent, "/workspace/../etc"},
		{"prefix but not subtree", dockerPersistent, "/workspacex"},
	}
	for _, tc := range refused {
		if _, err := ResolveBindingWorkDir(tc.cfg, tc.in); err == nil {
			t.Errorf("%s: accepted, want ErrInvalidBinding", tc.name)
		} else if !errors.As(err, &ErrInvalidBinding{}) {
			t.Errorf("%s: error type %T, want ErrInvalidBinding", tc.name, err)
		}
	}
}
