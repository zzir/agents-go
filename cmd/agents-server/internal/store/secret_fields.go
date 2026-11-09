package store

import (
	"encoding/json"
	"fmt"
)

// The credential fields of each entity, sealed at rest: each store installs
// its pair with withSecrets, and custom write paths wrap with sealedWrite.

// sealedWrite runs exec with m's credentials sealed, then opens them again
// whatever exec returned, so the caller keeps a plaintext struct.
func sealedWrite[T any](m *T, seal, open func(*T) error, exec func() error) error {
	if err := seal(m); err != nil {
		return err
	}
	err := exec()
	if oerr := open(m); err == nil {
		err = oerr
	}
	return err
}

// The labels every sealed value is bound to: "table.column", and for a
// credential inside a JSON column the field appended by sealJSONKeys.
const (
	labelProviderAPIKey       = "providers.api_key"
	labelProviderChatGPTToken = "providers.chatgpt_token"
	labelMcpOAuthToken        = "mcp_servers.oauth_token"
	labelMcpConfig            = "mcp_servers.config"
	labelSandboxConfig        = "sandboxes.config"
	labelWorkspaceEnv         = "projects.env" // the table's old name: renaming it would orphan every sealed env
	labelTriggerSecret        = "triggers.secret"
	labelSetting              = "settings.value"
)

func sealProvider(p *Provider) error {
	p.APIKey = sealSecret(labelProviderAPIKey, p.APIKey)
	p.ChatGPTToken = sealSecret(labelProviderChatGPTToken, p.ChatGPTToken)
	return nil
}

func openProvider(p *Provider) (err error) {
	if p.APIKey, err = openSecret(labelProviderAPIKey, p.APIKey); err != nil {
		return err
	}
	p.ChatGPTToken, err = openSecret(labelProviderChatGPTToken, p.ChatGPTToken)
	return err
}

// mcpSecretKeys are the credential fields inside an MCP server's transport
// config: the OAuth client secret and the headers (bearer tokens).
var mcpSecretKeys = []string{"oauth_client_secret", "headers"}

func sealMcpServer(m *McpServerConfig) (err error) {
	m.OAuthToken = sealSecret(labelMcpOAuthToken, m.OAuthToken)
	m.Config, err = sealJSONKeys(labelMcpConfig, m.Config, mcpSecretKeys...)
	return err
}

func openMcpServer(m *McpServerConfig) (err error) {
	if m.OAuthToken, err = openSecret(labelMcpOAuthToken, m.OAuthToken); err != nil {
		return err
	}
	m.Config, err = openJSONKeys(labelMcpConfig, m.Config, mcpSecretKeys...)
	return err
}

// SandboxSecretKeys are the credential fields inside a sandbox's config across
// every type (string fields and the e2b headers map): sealing and masking share it.
var SandboxSecretKeys = []string{"ssh_password", "api_key", "headers"}

func sealSandbox(sb *Sandbox) (err error) {
	sb.Config, err = sealJSONKeys(labelSandboxConfig, sb.Config, SandboxSecretKeys...)
	return err
}

func openSandbox(sb *Sandbox) (err error) {
	sb.Config, err = openJSONKeys(labelSandboxConfig, sb.Config, SandboxSecretKeys...)
	return err
}

// A workspace's environment is a credential surface like any other here: every
// value sealed at rest, none readable back (decisions §5.32).
func sealWorkspace(p *Workspace) error {
	return mapWorkspaceEnv(p, func(label, s string) (string, error) { return sealSecret(label, s), nil })
}

func openWorkspace(p *Workspace) error {
	return mapWorkspaceEnv(p, openSecret)
}

// mapWorkspaceEnv applies fn to every value, re-marshaling through []EnvVar so
// the payload keeps its canonical field order.
func mapWorkspaceEnv(p *Workspace, fn func(label, s string) (string, error)) error {
	if p.Env == "" || secretBox == nil && !hasSealed(json.RawMessage(p.Env)) {
		return nil
	}
	vars, err := DecodeWorkspaceEnv(p.Env)
	if err != nil {
		return err
	}
	// AAD is bound to the workspace id: another workspace's ciphertext does not open here.
	label := labelWorkspaceEnv + "." + p.ID
	for i, v := range vars {
		out, ferr := fn(label, v.Value)
		if ferr != nil {
			return fmt.Errorf("environment variable %q: %w", v.Key, ferr)
		}
		vars[i].Value = out
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return fmt.Errorf("encoding workspace environment: %w", err)
	}
	p.Env = string(b)
	return nil
}

func sealTrigger(t *Trigger) error {
	t.Secret = sealSecret(labelTriggerSecret, t.Secret)
	return nil
}

func openTrigger(t *Trigger) (err error) {
	t.Secret, err = openSecret(labelTriggerSecret, t.Secret)
	return err
}
