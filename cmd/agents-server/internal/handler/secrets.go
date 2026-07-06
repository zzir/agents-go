package handler

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SecretMask is the placeholder the API returns in place of stored secrets
// (API keys, passwords, auth headers). Secrets are write-only: a client that
// sends the mask back on update keeps the stored value, sends a new value to
// replace it, or sends "" to clear it. This lets the UI round-trip full
// objects without ever seeing the plaintext.
const SecretMask = "********"

// maskSecret returns the mask for a non-empty secret and "" otherwise.
func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	return SecretMask
}

// resolveSecret implements the write-side sentinel: the mask keeps the
// previously stored value, anything else (including "") is taken literally.
func resolveSecret(incoming, prev string) string {
	if incoming == SecretMask {
		return prev
	}
	return incoming
}

// maskFallbackModels masks the api_key of every entry in a fallback-models
// JSON array ([{model, api_key, base_url}]). Input that doesn't parse is
// returned unchanged.
func maskFallbackModels(raw string) string {
	if raw == "" {
		return ""
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return raw
	}
	changed := false
	for _, e := range entries {
		if s, ok := e["api_key"].(string); ok && s != "" {
			e["api_key"] = SecretMask
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return raw
	}
	return string(out)
}

// restoreFallbackModels resolves masked api_keys in an incoming
// fallback-models array against the previously stored one, matching entries
// by model name first and by position second.
func restoreFallbackModels(incoming, prev string) string {
	if incoming == "" || !strings.Contains(incoming, SecretMask) {
		return incoming
	}
	var in []map[string]any
	if err := json.Unmarshal([]byte(incoming), &in); err != nil {
		return incoming
	}
	var old []map[string]any
	_ = json.Unmarshal([]byte(prev), &old)
	prevKey := func(i int, model string) string {
		for _, o := range old {
			if om, _ := o["model"].(string); om == model {
				if k, ok := o["api_key"].(string); ok {
					return k
				}
			}
		}
		if i < len(old) {
			if k, ok := old[i]["api_key"].(string); ok {
				return k
			}
		}
		return ""
	}
	for i, e := range in {
		if s, ok := e["api_key"].(string); ok && s == SecretMask {
			model, _ := e["model"].(string)
			e["api_key"] = prevKey(i, model)
		}
	}
	out, err := json.Marshal(in)
	if err != nil {
		return incoming
	}
	return string(out)
}

// maskJSONFields masks the named string fields of a JSON object, plus every
// value of its "headers" object when maskHeaders is set. Unknown fields pass
// through untouched; input that doesn't parse is returned unchanged.
func maskJSONFields(raw json.RawMessage, maskHeaders bool, fields ...string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	changed := false
	if maskHeaders {
		if hdrs, ok := obj["headers"].(map[string]any); ok {
			for k, v := range hdrs {
				if s, ok := v.(string); ok && s != "" {
					hdrs[k] = SecretMask
					changed = true
				}
			}
		}
	}
	for _, f := range fields {
		if s, ok := obj[f].(string); ok && s != "" {
			obj[f] = SecretMask
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// restoreJSONFields resolves masked values in an incoming JSON object against
// the previously stored one: the named fields, plus per-key "headers" values
// when restoreHeaders is set. A masked value whose previous counterpart is
// missing resolves to "".
func restoreJSONFields(incoming, prev json.RawMessage, restoreHeaders bool, fields ...string) json.RawMessage {
	if len(incoming) == 0 || !bytes.Contains(incoming, []byte(SecretMask)) {
		return incoming
	}
	var in map[string]any
	if err := json.Unmarshal(incoming, &in); err != nil {
		return incoming
	}
	var old map[string]any
	_ = json.Unmarshal(prev, &old)
	if restoreHeaders {
		oldHeaders, _ := old["headers"].(map[string]any)
		if hdrs, ok := in["headers"].(map[string]any); ok {
			for k, v := range hdrs {
				if s, ok := v.(string); ok && s == SecretMask {
					if ov, ok := oldHeaders[k].(string); ok {
						hdrs[k] = ov
					} else {
						hdrs[k] = ""
					}
				}
			}
		}
	}
	for _, f := range fields {
		if s, ok := in[f].(string); ok && s == SecretMask {
			if ov, ok := old[f].(string); ok {
				in[f] = ov
			} else {
				in[f] = ""
			}
		}
	}
	out, err := json.Marshal(in)
	if err != nil {
		return incoming
	}
	return out
}

// sanitizeMcpConfig returns cfg with its transport secrets masked: header
// values and the OAuth client secret for streamable_http servers. stdio
// configs carry no secrets and pass through.
func sanitizeMcpConfig(cfg store.McpServerConfig) store.McpServerConfig {
	if cfg.TransportType == "streamable_http" {
		cfg.Config = maskJSONFields(cfg.Config, true, "oauth_client_secret")
	}
	return cfg
}

// restoreMcpConfig resolves masked transport secrets in an incoming config
// against the previously stored config.
func restoreMcpConfig(transportType string, incoming, prev json.RawMessage) json.RawMessage {
	if transportType != "streamable_http" {
		return incoming
	}
	return restoreJSONFields(incoming, prev, true, "oauth_client_secret")
}

// sanitizeSandboxConfig returns cfg with backend secrets masked (the SSH
// password); local and docker configs carry no secrets.
func sanitizeSandboxConfig(cfg store.SandboxConfig) store.SandboxConfig {
	if cfg.Type == "ssh" {
		cfg.Config = maskJSONFields(cfg.Config, false, "password")
	}
	return cfg
}

// restoreSandboxConfig resolves a masked SSH password in an incoming config
// against the previously stored config.
func restoreSandboxConfig(typ string, incoming, prev json.RawMessage) json.RawMessage {
	if typ != "ssh" {
		return incoming
	}
	return restoreJSONFields(incoming, prev, false, "password")
}

// sanitizeAgentConfig masks the secret-bearing fields of an agent config for
// API responses: the provider API key and the per-entry fallback-model keys.
func sanitizeAgentConfig(ac *store.AgentConfig) {
	ac.APIKey = maskSecret(ac.APIKey)
	ac.FallbackModels = maskFallbackModels(ac.FallbackModels)
	ac.ChatGPTToken = maskSecret(ac.ChatGPTToken)
}

// secretSettingKeys are the settings whose values are secrets and therefore
// masked on read and sentinel-resolved on write.
var secretSettingKeys = map[string]bool{
	"brave_api_key": true,
	// Fallback provider key used when an agent has no api_key of its own
	// (read in bridge.buildAgentFromConfig).
	"openai_api_key": true,
}
