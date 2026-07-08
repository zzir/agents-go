package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
	sshsb "github.com/zzir/agents-go/sandbox/ssh"
)

// SandboxManager caches and reuses sandbox instances keyed by config id.
type SandboxManager struct {
	mu        sync.RWMutex
	sandboxes map[string]sandbox.Sandbox
	workspace string
	// trust holds per-session exec_command approval grants, consulted by the
	// commandGate and updated by the approval resolver.
	trust *TrustStore
}

// NewSandboxManager creates a SandboxManager that roots local sandboxes at workspace.
func NewSandboxManager(workspace string) *SandboxManager {
	return &SandboxManager{
		sandboxes: make(map[string]sandbox.Sandbox),
		workspace: workspace,
		trust:     NewTrustStore(),
	}
}

// Trust exposes the session command-trust store so the approval resolver can
// record "allow this command" / "allow all" grants for a session.
func (m *SandboxManager) Trust() *TrustStore { return m.trust }

// commandGate is exec_command's per-call approval gate: approval is required
// unless the run's session has already trusted this exact command (or all
// commands). The session id rides in RunContext.Context, set by the runner.
func (m *SandboxManager) commandGate(_ context.Context, rc *agents.RunContext, argsJSON string, _ string) (bool, error) {
	if rc == nil {
		return true, nil
	}
	sid, _ := rc.Context.(string)
	if sid == "" {
		return true, nil // no session context → be safe, require approval
	}
	return !m.trust.forSession(sid).trusted(commandHash(argsJSON)), nil
}

// GetOrCreate returns the cached sandbox for the config, building and caching one if absent.
func (m *SandboxManager) GetOrCreate(cfg *store.SandboxConfig) (sandbox.Sandbox, error) {
	m.mu.RLock()
	if sb, ok := m.sandboxes[cfg.ID]; ok {
		m.mu.RUnlock()
		return sb, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if sb, ok := m.sandboxes[cfg.ID]; ok {
		return sb, nil
	}

	sb, err := m.buildSandbox(cfg)
	if err != nil {
		return nil, err
	}
	m.sandboxes[cfg.ID] = sb
	return sb, nil
}

// Remove closes and evicts the cached sandbox with the given id, if present.
func (m *SandboxManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		_ = sb.Close()
		delete(m.sandboxes, id)
	}
}

// CloseAll closes and evicts every cached sandbox.
func (m *SandboxManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, sb := range m.sandboxes {
		_ = sb.Close()
		delete(m.sandboxes, id)
	}
}

// CodeTool builds a code-execution tool bound to the sandbox for the given config.
func (m *SandboxManager) CodeTool(cfg *store.SandboxConfig) (agents.Tool, error) {
	sb, err := m.GetOrCreate(cfg)
	if err != nil {
		return nil, err
	}
	return sandbox.CodeTool(sb, sandbox.CodeToolConfig{}), nil
}

// SandboxTools returns exec_command plus read_file, write_file, list_files and
// apply_patch tools for the given sandbox config. apply_patch (Codex-style
// multi-file edits) and the file tools all edit through the same Sandbox, so
// they target the same filesystem exec_command runs in. When commandApproval is
// set, exec_command is gated per call through the session command-trust store:
// a command is approved on first use, then trusted per the user's choice.
func (m *SandboxManager) SandboxTools(cfg *store.SandboxConfig, commandApproval bool) ([]agents.Tool, error) {
	sb, err := m.GetOrCreate(cfg)
	if err != nil {
		return nil, err
	}
	codeCfg := sandbox.CodeToolConfig{}
	if commandApproval {
		codeCfg.NeedsApprovalFunc = m.commandGate
	}
	tools := []agents.Tool{sandbox.CodeTool(sb, codeCfg)}
	tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)
	tools = append(tools, sandbox.ApplyPatchTool(sb, sandbox.FileToolConfig{}))
	return tools, nil
}

func (m *SandboxManager) buildSandbox(cfg *store.SandboxConfig) (sandbox.Sandbox, error) {
	switch cfg.Type {
	case "local":
		var lc store.LocalConfig
		if err := unmarshalConfig(cfg.Config, &lc); err != nil {
			return nil, fmt.Errorf("local sandbox: invalid config: %w", err)
		}
		opts := sandbox.LocalOptions{MaxReadFileBytes: lc.MaxReadFileBytes}
		if m.workspace != "" {
			opts.WorkDir = m.workspace
		}
		return sandbox.NewLocalWithOptions(opts), nil
	case "docker":
		var dc store.DockerConfig
		if err := unmarshalConfig(cfg.Config, &dc); err != nil {
			return nil, fmt.Errorf("docker sandbox: invalid config: %w", err)
		}
		if dc.Image == "" {
			return nil, fmt.Errorf("docker sandbox requires an image")
		}
		opts := dockersb.Options{
			Image:            dc.Image,
			Runtime:          dc.Runtime,
			Network:          dc.Network,
			Persistent:       dc.Persistent,
			ContainerName:    dc.ContainerName,
			MaxReadFileBytes: dc.MaxReadFileBytes,
		}
		if dc.Persistent && m.workspace != "" {
			opts.WorkDir = m.workspace
		}
		return dockersb.New(opts)
	case "ssh":
		var sc store.SSHConfig
		if err := unmarshalConfig(cfg.Config, &sc); err != nil {
			return nil, fmt.Errorf("ssh sandbox: invalid config: %w", err)
		}
		if sc.Addr == "" {
			return nil, fmt.Errorf("ssh sandbox requires a host")
		}
		if sc.User == "" {
			return nil, fmt.Errorf("ssh sandbox requires a user")
		}
		return sshsb.New(sshsb.Options{
			Addr: sc.Addr,
			User: sc.User,
			Auth: sshsb.AuthConfig{
				UseAgent: sc.UseAgent,
				KeyFile:  sc.KeyFile,
				Password: sc.Password,
			},
			HostKey: sshsb.HostKeyConfig{
				KnownHostsFile:        sc.KnownHosts,
				InsecureIgnoreHostKey: sc.InsecureHostKey,
			},
			WorkDir:          sc.WorkDir,
			MaxReadFileBytes: sc.MaxReadFileBytes,
		})
	default:
		return nil, fmt.Errorf("unknown sandbox type: %s", cfg.Type)
	}
}

// unmarshalConfig decodes a SandboxConfig.Config payload, treating empty as a
// zero-value config so the per-type required-field checks produce the error.
func unmarshalConfig(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
