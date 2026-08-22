package secrets

import (
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := ParseKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal("providers.api_key", "sk-secret")
	if !strings.HasPrefix(sealed, "enc:v2:"+box.KeyID()+":") || sealed == "sk-secret" {
		t.Fatalf("sealed = %q", sealed)
	}
	got, err := box.Open("providers.api_key", sealed)
	if err != nil || got != "sk-secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Bound to its place: the same ciphertext in another column does not open.
	if _, err := box.Open("mcp_servers.config.headers", sealed); err == nil {
		t.Fatal("a sealed value moved to another column must not open there")
	}
	// A pasted ciphertext is text: sealed again, and opens to the text pasted,
	// never to what the ciphertext held.
	again := box.Seal("mcp_servers.config.headers", sealed)
	if again == sealed {
		t.Fatal("a value that looks sealed must be sealed as the text it is")
	}
	if got, _ := box.Open("mcp_servers.config.headers", again); got != sealed {
		t.Fatalf("re-sealed input opens to %q, want the pasted text", got)
	}
	// Plaintext from before a key passes through; "" stays "".
	if got, _ := box.Open("providers.api_key", "legacy"); got != "legacy" {
		t.Fatal("unsealed values must pass through")
	}
	if box.Seal("providers.api_key", "") != "" {
		t.Fatal("an absent secret stays absent")
	}

	otherKey := make([]byte, 32)
	otherKey[0] = 1
	other, _ := New(otherKey)
	_, err = other.Open("providers.api_key", sealed)
	if err == nil || !strings.Contains(err.Error(), box.KeyID()) || !strings.Contains(err.Error(), other.KeyID()) {
		t.Fatalf("the wrong key must fail naming both key ids, got %v", err)
	}
	var none *Box
	if none.Seal("providers.api_key", "x") != "x" {
		t.Fatal("no key: seal passes through")
	}
	if _, err := none.Open("providers.api_key", sealed); err == nil {
		t.Fatal("no key: a sealed value must be an error, not ciphertext read as a credential")
	}
	if none.KeyID() != "" {
		t.Fatal("no key: no key id")
	}
}
