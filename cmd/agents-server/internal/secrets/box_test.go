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
	sealed := box.Seal("sk-secret")
	if !strings.HasPrefix(sealed, prefix) || sealed == "sk-secret" {
		t.Fatalf("sealed = %q", sealed)
	}
	if box.Seal(sealed) != sealed {
		t.Fatal("sealing twice must not double-wrap")
	}
	got, err := box.Open(sealed)
	if err != nil || got != "sk-secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Plaintext from before a key passes through; "" stays "".
	if got, _ := box.Open("legacy"); got != "legacy" {
		t.Fatal("unsealed values must pass through")
	}
	if box.Seal("") != "" {
		t.Fatal("an absent secret stays absent")
	}

	otherKey := make([]byte, 32)
	otherKey[0] = 1
	other, _ := New(otherKey)
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("the wrong key must fail to open")
	}
	var none *Box
	if none.Seal("x") != "x" {
		t.Fatal("no key: seal passes through")
	}
	if _, err := none.Open(sealed); err == nil {
		t.Fatal("no key: a sealed value must be an error, not ciphertext read as a credential")
	}
}
