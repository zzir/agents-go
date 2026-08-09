package bridge

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The binding-time workdir rules, per backend: what canonicalizes, what is
// refused. Run-time leniency (effectiveWorkDir) is covered separately — these
// rules gate only NEW bindings.
func TestResolveBindingWorkDir(t *testing.T) {
	cfg := func(typ, config string) *store.SandboxConfig {
		return &store.SandboxConfig{Type: typ, Config: json.RawMessage(config)}
	}
	sshBare := cfg("ssh", `{"addr":"h","user":"u"}`)
	sshHome := cfg("ssh", `{"addr":"h","user":"u","work_dir":"/srv"}`)
	sshRelHome := cfg("ssh", `{"addr":"h","user":"u","work_dir":"projects"}`)
	dockerEphemeral := cfg("docker", `{"image":"i"}`)
	dockerPersistent := cfg("docker", `{"image":"i","persistent":true}`)
	local := cfg("local", `{}`)

	ok := []struct {
		name string
		cfg  *store.SandboxConfig
		in   string
		want string
	}{
		{"local empty", local, "", ""},
		{"local absolute cleans", local, "/a/b/../c/", "/a/c"},
		{"ssh explicit dir", sshBare, "/srv/app", "/srv/app"},
		{"ssh cleans", sshBare, "/srv//app/./x", "/srv/app/x"},
		{"ssh empty over a config default", sshHome, "", ""},
		{"docker ephemeral empty", dockerEphemeral, "", ""},
		{"docker ephemeral /workspace is the default", dockerEphemeral, "/workspace", ""},
		{"docker persistent empty", dockerPersistent, "", ""},
		{"docker /workspace itself is the default", dockerPersistent, "/workspace", ""},
		{"docker /workspace/ normalizes to the default", dockerPersistent, "/workspace/", ""},
		{"docker subtree cleans", dockerPersistent, "/workspace//proj/", "/workspace/proj"},
		{"whitespace trims", sshBare, "  /srv/app  ", "/srv/app"},
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
		{"local relative", local, "rel/path"},
		{"ssh no dir anywhere", sshBare, ""},
		{"ssh relative dir", sshBare, "projects/app"},
		{"ssh empty over a RELATIVE config default", sshRelHome, ""},
		{"docker ephemeral any dir", dockerEphemeral, "/workspace/x"},
		{"docker outside /workspace", dockerPersistent, "/tmp/project"},
		{"docker escape via ..", dockerPersistent, "/workspace/../etc"},
		{"docker prefix but not subtree", dockerPersistent, "/workspacex"},
	}
	for _, tc := range refused {
		if _, err := ResolveBindingWorkDir(tc.cfg, tc.in); err == nil {
			t.Errorf("%s: accepted, want ErrInvalidBinding", tc.name)
		} else if !errors.As(err, &ErrInvalidBinding{}) {
			t.Errorf("%s: error type %T, want ErrInvalidBinding", tc.name, err)
		}
	}
}
