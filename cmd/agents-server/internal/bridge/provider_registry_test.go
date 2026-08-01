package bridge

import (
	"slices"
	"strings"
	"testing"
)

// The registry is the single source for provider selection; these invariants
// are what every derived surface (validation, secrets masking, the
// provider-types endpoint) silently relies on.
func TestProviderRegistryInvariants(t *testing.T) {
	if len(providerDefs) == 0 {
		t.Fatal("empty registry")
	}
	if providerDefs[0].Type != ProviderTypeOpenAI {
		t.Errorf("first entry = %q — openai is the default UIs rely on", providerDefs[0].Type)
	}
	seen := map[string]bool{}
	for _, d := range providerDefs {
		if d.Type == "" || seen[d.Type] {
			t.Errorf("type %q is empty or duplicated", d.Type)
		}
		seen[d.Type] = true
		if !strings.HasSuffix(d.SettingKey, "_api_key") {
			t.Errorf("%s: SettingKey %q must end in _api_key (masked as a secret by convention)", d.Type, d.SettingKey)
		}
		if d.Build == nil || d.BuildAgent == nil {
			t.Errorf("%s: nil constructor", d.Type)
		}
		if d.Capabilities.Provider != d.Type {
			t.Errorf("%s: Capabilities.Provider = %q — the declaration must be the adapter's own", d.Type, d.Capabilities.Provider)
		}
	}
}

func TestProviderDefFor(t *testing.T) {
	def, err := providerDefFor("")
	if err != nil || def.Type != ProviderTypeOpenAI {
		t.Fatalf("empty selector = %q/%v, want openai", def.Type, err)
	}
	if _, err := providerDefFor(ProviderTypeAnthropic); err != nil {
		t.Fatal(err)
	}
	_, err = providerDefFor("gemini")
	if err == nil || !strings.Contains(err.Error(), "openai, anthropic") {
		t.Fatalf("unknown selector error = %v, want the valid set named", err)
	}
}

func TestProviderTypesServesMachineFacts(t *testing.T) {
	infos := ProviderTypes()
	byType := map[string]ProviderTypeInfo{}
	for _, info := range infos {
		byType[info.Type] = info
	}
	oa := byType[ProviderTypeOpenAI]
	if !slices.Contains(oa.AuthModes, authModeChatGPTLogin) {
		t.Errorf("openai auth_modes = %v, want chatgpt_login listed", oa.AuthModes)
	}
	ant := byType[ProviderTypeAnthropic]
	if !slices.Contains(ant.Unsupported, "service_tier") {
		t.Errorf("anthropic unsupported = %v, want the adapter's declaration (service_tier missing)", ant.Unsupported)
	}
	if oa.SettingKey != "openai_api_key" || ant.SettingKey != "anthropic_api_key" {
		t.Errorf("setting keys = %q/%q", oa.SettingKey, ant.SettingKey)
	}
}
