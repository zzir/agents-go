package store

import "encoding/json"

// The credential fields of each entity, sealed at rest. Each store installs
// its pair with withSecrets, and its custom write paths wrap with sealedWrite
// — the caller's value is plaintext before and after every store call.

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
	labelSandboxConfig        = "sandbox_configs.config"
	labelTriggerSecret        = "triggers.secret"
	labelAgentFallbackModels  = "agent_configs.fallback_models"
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
// config: a pre-registered OAuth client secret and the headers, which carry
// bearer tokens.
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

func sealSandbox(c *SandboxConfig) (err error) {
	c.Config, err = sealJSONKeys(labelSandboxConfig, c.Config, "password")
	return err
}

func openSandbox(c *SandboxConfig) (err error) {
	c.Config, err = openJSONKeys(labelSandboxConfig, c.Config, "password")
	return err
}

func sealTrigger(t *Trigger) error {
	t.Secret = sealSecret(labelTriggerSecret, t.Secret)
	return nil
}

func openTrigger(t *Trigger) (err error) {
	t.Secret, err = openSecret(labelTriggerSecret, t.Secret)
	return err
}

// An agent's fallback chain is a JSON array of entries each carrying its own
// api_key.
func sealAgentConfig(ac *AgentConfig) (err error) {
	ac.Resilience.FallbackModels, err = mapJSONArrayKey(labelAgentFallbackModels, ac.Resilience.FallbackModels, "api_key", func(l, s string) (string, error) { return sealSecret(l, s), nil })
	return err
}

func openAgentConfig(ac *AgentConfig) (err error) {
	ac.Resilience.FallbackModels, err = mapJSONArrayKey(labelAgentFallbackModels, ac.Resilience.FallbackModels, "api_key", openSecret)
	return err
}

// mapJSONArrayKey applies fn to the named string field of every object in a
// JSON array (the fallback chain); anything else passes through.
func mapJSONArrayKey(label, raw, key string, fn func(label, s string) (string, error)) (string, error) {
	if raw == "" || secretBox == nil && !hasSealed(json.RawMessage(raw)) {
		return raw, nil
	}
	var items []json.RawMessage
	if json.Unmarshal([]byte(raw), &items) != nil {
		return raw, nil //nolint:nilerr // not an array: nothing here to seal, the value passes through
	}
	for i, item := range items {
		out, err := mapJSONKeys(label, item, fn, key)
		if err != nil {
			return "", err
		}
		items[i] = out
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
