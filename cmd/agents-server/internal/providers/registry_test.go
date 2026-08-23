package providers

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
	if providerDefs[0].Type != TypeOpenAI {
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
		if d.Build == nil {
			t.Errorf("%s: nil constructor", d.Type)
		}
	}
}

func TestProviderDefFor(t *testing.T) {
	def, err := DefFor("")
	if err != nil || def.Type != TypeOpenAI {
		t.Fatalf("empty selector = %q/%v, want openai", def.Type, err)
	}
	if _, err := DefFor(TypeAnthropic); err != nil {
		t.Fatal(err)
	}
	_, err = DefFor("gemini")
	if err == nil || !strings.Contains(err.Error(), "openai, anthropic") {
		t.Fatalf("unknown selector error = %v, want the valid set named", err)
	}
}

func TestProviderTypesServesMachineFacts(t *testing.T) {
	infos := Types()
	byType := map[string]TypeInfo{}
	for _, info := range infos {
		byType[info.Type] = info
	}
	oa := byType[TypeOpenAI]
	if !slices.Contains(oa.AuthModes, AuthModeChatGPTLogin) {
		t.Errorf("openai auth_modes = %v, want chatgpt_login listed", oa.AuthModes)
	}
	ant := byType[TypeAnthropic]
	if !slices.Contains(ant.Unsupported, "service_tier") {
		t.Errorf("anthropic unsupported = %v, want the adapter's declaration (service_tier missing)", ant.Unsupported)
	}
	if oa.SettingKey != "openai_api_key" || ant.SettingKey != "anthropic_api_key" {
		t.Errorf("setting keys = %q/%q", oa.SettingKey, ant.SettingKey)
	}
}
