package handler

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SecretMask is the placeholder the API returns in place of stored secrets.
// Sent back on update it keeps the stored value; "" clears it — invariant 9.
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

// normalizeEndpoint canonicalizes a base_url for credential-identity
// comparison — trailing slash and whitespace only, never anything that could equate two endpoints.
func normalizeEndpoint(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// credentialTargetChanged reports whether a stored api_key's destination, the
// (provider_type, base_url) pair, differs between the row and the update (invariant 9).
func credentialTargetChanged(prevProvider, prevBaseURL, newProvider, newBaseURL string) bool {
	return providers.NormalizeType(prevProvider) != providers.NormalizeType(newProvider) ||
		normalizeEndpoint(prevBaseURL) != normalizeEndpoint(newBaseURL)
}

// maskFallbackModels masks the api_key of every entry in a fallback-models
// JSON array; input that doesn't parse is returned unchanged.
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

// restoreFallbackModels resolves masked api_keys against the stored array by
// (normalized provider_type, normalized base_url, model), never by position — invariant 9.
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
		return providers.NormalizeType(p) + "\x00" + normalizeEndpoint(u) + "\x00" + m
	}
	// Keys queue PER IDENTITY and are consumed in order, so two same-identity
	// entries each keep their own key.
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
// "headers" value when maskHeaders is set; unparseable input is returned as is.
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

// restoreJSONFields resolves masked values against the stored JSON object
// (the named fields, plus "headers" when restoreHeaders); no counterpart is "".
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

// sanitizeMcpConfig returns cfg with its secrets masked: header values and
// the OAuth client secret.
func sanitizeMcpConfig(cfg store.McpServerConfig) store.McpServerConfig {
	cfg.Config = maskJSONFields(cfg.Config, true, "oauth_client_secret")
	return cfg
}

// restoreMcpConfig resolves masked secrets in an incoming config against the
// previously stored config.
func restoreMcpConfig(incoming, prev json.RawMessage) json.RawMessage {
	return restoreJSONFields(incoming, prev, true, "oauth_client_secret")
}

// storedSandboxSecret reports whether a stored sandbox actually holds a
// credential — the mask-across-destination refusal only applies when it does.
func storedSandboxSecret(prev json.RawMessage) bool {
	var cfg struct {
		SSHPassword string `json:"ssh_password"`
		APIKey      string `json:"api_key"`
	}
	return json.Unmarshal(prev, &cfg) == nil && (cfg.SSHPassword != "" || cfg.APIKey != "")
}

// maskAcrossDestination reports whether incoming still carries the mask
// while the JSON field naming the secret's destination changed — invariant 9.
func maskAcrossDestination(incoming, prev json.RawMessage, field string) bool {
	if !bytes.Contains(incoming, []byte(SecretMask)) {
		return false
	}
	var in, old map[string]any
	if err := json.Unmarshal(incoming, &in); err != nil {
		return false
	}
	_ = json.Unmarshal(prev, &old)
	is, _ := in[field].(string)
	os, _ := old[field].(string)
	return is != os
}

// sanitizeSandboxConfig returns sb shaped for a response: the credential (an
// SSH password, or a service's API key) masked, the type's supports filled.
func sanitizeSandboxConfig(sb store.Sandbox) store.Sandbox {
	sb.Config = maskJSONFields(sb.Config, false, sandboxSecretFields...)
	sb.Supports = store.SandboxSupportsFor(sb.Type)
	return sb
}

// restoreSandboxConfig resolves a masked credential in an incoming sandbox
// config against the previously stored one.
func restoreSandboxConfig(incoming, prev json.RawMessage) json.RawMessage {
	return restoreJSONFields(incoming, prev, false, sandboxSecretFields...)
}

// sandboxSecretFields are every sandbox type's credential fields — one list,
// mirroring the store's sealing list.
var sandboxSecretFields = []string{"ssh_password", "api_key"}

// sanitizeAgentConfig masks the per-entry fallback-model keys for API
// responses (the provider credential lives on the Provider entity).
func sanitizeAgentConfig(ac *store.AgentConfig) {
	ac.Resilience.FallbackModels = maskFallbackModels(ac.Resilience.FallbackModels)
}

// sanitizeProvider masks a provider's key for API responses and projects the
// ChatGPT token into the logged-in signal — the ONE place a model key is masked.
func sanitizeProvider(pv *store.Provider) {
	pv.APIKey = maskSecret(pv.APIKey)
	pv.ChatGPTLoggedIn = pv.ChatGPTToken != ""
	pv.ChatGPTToken = "" // json:"-" already hides it; cleared as defense in depth
}
