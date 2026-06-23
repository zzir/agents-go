package bridge

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
	rootDir   string
}

// NewSandboxManager creates a SandboxManager that roots local sandboxes at rootDir.
func NewSandboxManager(rootDir string) *SandboxManager {
	return &SandboxManager{
		sandboxes: make(map[string]sandbox.Sandbox),
		rootDir:   rootDir,
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
	toolCfg := sandbox.CodeToolConfig{
		Filename: cfg.Filename,
		RunCmd:   []string{"python3", "main.py"},
	}
	if cfg.Filename != "" {
		toolCfg.RunCmd = []string{"python3", cfg.Filename}
	}
	if cfg.RunCmd != "" {
		var cmd []string
		if err := json.Unmarshal([]byte(cfg.RunCmd), &cmd); err == nil && len(cmd) > 0 {
			toolCfg.RunCmd = cmd
		}
	}
	if cfg.Timeout > 0 {
		toolCfg.Timeout = time.Duration(cfg.Timeout) * time.Second
	}
	return sandbox.CodeTool(sb, toolCfg), nil
}

func (m *SandboxManager) buildSandbox(cfg *store.SandboxConfig) (sandbox.Sandbox, error) {
	switch cfg.Type {
	case "local":
		if m.rootDir != "" {
			return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: m.rootDir}), nil
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
		return dockersb.New(dockersb.Options{
			Image:   dc.Image,
			Host:    dc.Host,
			Network: dc.Network,
		})
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
