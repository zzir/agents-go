package sandboxes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

func execTool(t *testing.T, tools []*agents.Tool) *agents.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == "exec_command" {
			return tool
		}
	}
	t.Fatal("exec_command not among sandbox tools")
	return nil
}

// exec_command advertises session_id only where a shell can actually be held
// open — terminal-capable backends (ssh, persistent docker). Elsewhere the
// schema must not promise persistence the backend cannot deliver.
func TestSandboxToolsSessionSchemaPerBackend(t *testing.T) {
	cases := []struct {
		name string
		cfg  *store.SandboxConfig
		want bool
	}{
		{"local", &store.SandboxConfig{ID: "l", Type: "local"}, false},
		{"ssh", &store.SandboxConfig{ID: "s", Type: "ssh"}, true},
		{"docker persistent", &store.SandboxConfig{ID: "dp", Type: "docker", Config: json.RawMessage(`{"image":"i","persistent":true}`)}, true},
		{"docker ephemeral", &store.SandboxConfig{ID: "de", Type: "docker", Config: json.RawMessage(`{"image":"i"}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			m.buildOverride = func(*store.SandboxConfig, string) (sandbox.Sandbox, error) {
				return &closeCountingSandbox{}, nil
			}
			tools, release, err := m.SandboxTools(tc.cfg, "", false)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			raw, err := json.Marshal(execTool(t, tools).ParamsJSONSchema)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(raw), "session_id"); got != tc.want {
				t.Errorf("schema mentions session_id = %v, want %v: %s", got, tc.want, raw)
			}
		})
	}
}

// The named shells a run opens are scoped to its toolset: the release returned
// by SandboxTools closes the pool, so a late session command fails instead of
// opening a shell nobody will ever close.
func TestSandboxToolsReleaseClosesSessionPool(t *testing.T) {
	m := NewManager(t.TempDir())
	m.buildOverride = func(*store.SandboxConfig, string) (sandbox.Sandbox, error) {
		return &closeCountingSandbox{}, nil
	}
	cfg := &store.SandboxConfig{ID: "s", Type: "ssh"}
	tools, release, err := m.SandboxTools(cfg, "", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	out, err := execTool(t, tools).OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"echo x","timeout_seconds":0,"workdir":"","session_id":"build"}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "session pool is closed") {
		t.Errorf("output = %q, want the closed-pool refusal", s)
	}
}
