package handler

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
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

// normalizeEndpoint canonicalizes a base_url for CREDENTIAL IDENTITY
// comparison — trailing-slash and whitespace variants of the same endpoint
// must not read as a change. It deliberately does no more than that: a
// too-eager normalization that equated two genuinely different endpoints
// would restore a key across them, and false "changed" positives merely ask
// the user to re-enter the key.
func normalizeEndpoint(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// credentialTargetChanged reports whether a stored api_key's DESTINATION —
// the (provider_type, base_url) pair — differs between the stored row and
// the incoming update. A masked key must not round-trip across it: the mask
// means "keep the key I stored", and the key was stored for that
// destination, not for wherever the config points now.
func credentialTargetChanged(prevProvider, prevBaseURL, newProvider, newBaseURL string) bool {
	return bridge.NormalizeProviderType(prevProvider) != bridge.NormalizeProviderType(newProvider) ||
		normalizeEndpoint(prevBaseURL) != normalizeEndpoint(newBaseURL)
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
// fallback-models array against the previously stored one. An entry's key is
// restored ONLY from a stored entry with the same (normalized provider_type,
// normalized base_url, model) — never across providers OR endpoints, and
// never by position: any looser match hands one backend's real credential to
// another after the entry is edited (two same-provider entries pointed at
// different OpenAI-compatible endpoints are different credentials). A mask
// with no such match resolves to "" (the entry falls back to the global
// per-provider key), which is the safe direction.
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
	entryIdentity := func(e map[string]any) string {
		p, _ := e["provider_type"].(string)
		u, _ := e["base_url"].(string)
		m, _ := e["model"].(string)
		return bridge.NormalizeProviderType(p) + "\x00" + normalizeEndpoint(u) + "\x00" + m
	}
	// Keys queue PER IDENTITY and are consumed in order: two same-identity
	// entries (key rotation against one endpoint) each keep their own key
	// instead of both collapsing onto the first.
	prevKeys := map[string][]string{}
	for _, o := range old {
		if k, ok := o["api_key"].(string); ok {
			id := entryIdentity(o)
			prevKeys[id] = append(prevKeys[id], k)
		}
	}
	for _, e := range in {
		if s, ok := e["api_key"].(string); ok && s == SecretMask {
			id := entryIdentity(e)
			if q := prevKeys[id]; len(q) > 0 {
				e["api_key"] = q[0]
				prevKeys[id] = q[1:]
			} else {
				e["api_key"] = ""
			}
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
// API responses (the provider API key and the per-entry fallback-model keys)
// and projects the ChatGPT token into the derived logged-in signal — the token
// itself never leaves the server.
func sanitizeAgentConfig(ac *store.AgentConfig) {
	ac.Provider.APIKey = maskSecret(ac.Provider.APIKey)
	ac.Resilience.FallbackModels = maskFallbackModels(ac.Resilience.FallbackModels)
	ac.ChatGPTLoggedIn = ac.ChatGPTToken != ""
	ac.ChatGPTToken = "" // json:"-" already hides it; cleared as defense in depth
}

// secretSettingKeys are the settings whose values are secrets and therefore
// masked on read and sentinel-resolved on write.
var secretSettingKeys = map[string]bool{
	"brave_api_key": true,
}

// The per-provider global fallback keys ("openai_api_key", …) are derived
// from the provider registry, so a new backend cannot add a key setting and
// forget to mask it.
func init() {
	for _, p := range bridge.ProviderTypes() {
		secretSettingKeys[p.SettingKey] = true
	}
}
