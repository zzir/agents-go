package bridge

import (
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
}

// NewSandboxManager creates a SandboxManager that roots local sandboxes at workspace.
func NewSandboxManager(workspace string) *SandboxManager {
	return &SandboxManager{
		sandboxes: make(map[string]sandbox.Sandbox),
		workspace: workspace,
	}
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

// SandboxTools returns exec_command plus read_file, write_file and list_files
// tools for the given sandbox config.
func (m *SandboxManager) SandboxTools(cfg *store.SandboxConfig) ([]agents.Tool, error) {
	sb, err := m.GetOrCreate(cfg)
	if err != nil {
		return nil, err
	}
	tools := []agents.Tool{sandbox.CodeTool(sb, sandbox.CodeToolConfig{})}
	tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)
	return tools, nil
}

func (m *SandboxManager) buildSandbox(cfg *store.SandboxConfig) (sandbox.Sandbox, error) {
	switch cfg.Type {
	case "local":
		if m.workspace != "" {
			return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: m.workspace}), nil
		}
		return sandbox.NewLocal(), nil
	case "docker":
		var dc store.DockerConfig
		if err := unmarshalConfig(cfg.Config, &dc); err != nil {
			return nil, fmt.Errorf("docker sandbox: invalid config: %w", err)
		}
		if dc.Image == "" {
			return nil, fmt.Errorf("docker sandbox requires an image")
		}
		opts := dockersb.Options{
			Image:         dc.Image,
			Runtime:       dc.Runtime,
			Network:       dc.Network,
			Persistent:    dc.Persistent,
			ContainerName: dc.ContainerName,
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
			WorkDir: sc.WorkDir,
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
