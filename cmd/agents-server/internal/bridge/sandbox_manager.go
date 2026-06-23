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
		if cfg.Image == "" {
			return nil, fmt.Errorf("docker sandbox requires an image")
		}
		return dockersb.New(dockersb.Options{
			Image:   cfg.Image,
			Host:    cfg.Host,
			Network: cfg.Network,
		})
	default:
		return nil, fmt.Errorf("unknown sandbox type: %s", cfg.Type)
	}
}
