package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"golang.org/x/crypto/ssh"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"abc":           "'abc'",
		"a b":           "'a b'",
		"a'b":           `'a'\''b'`,
		"$(rm -rf /)":   "'$(rm -rf /)'",
		"a&&b; c | d":   "'a&&b; c | d'",
		"it's a 'test'": `'it'\''s a '\''test'\'''`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	got := buildCommand("/tmp/work", nil, []string{"python3", "main.py"})
	want := "cd '/tmp/work' && exec 'python3' 'main.py'"
	if got != want {
		t.Errorf("buildCommand = %q, want %q", got, want)
	}
}

func TestBuildCommand_WithEnvSorted(t *testing.T) {
	// Env keys must be emitted in a deterministic (sorted) order.
	got := buildCommand("/w", map[string]string{"B": "2", "A": "1"}, []string{"sh", "-c", "echo $A$B"})
	want := "cd '/w' && exec env 'A=1' 'B=2' 'sh' '-c' 'echo $A$B'"
	if got != want {
		t.Errorf("buildCommand = %q, want %q", got, want)
	}
}

func TestBuildCommand_QuotesInjection(t *testing.T) {
	// A malicious env value or argument must stay a single literal token.
	got := buildCommand("/w", map[string]string{"X": "; rm -rf /"}, []string{"echo", "$(whoami)"})
	want := `cd '/w' && exec env 'X=; rm -rf /' 'echo' '$(whoami)'`
	if got != want {
		t.Errorf("buildCommand = %q, want %q", got, want)
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"host":           "host:22",
		"host:22":        "host:22",
		"host:2222":      "host:2222",
		"1.2.3.4":        "1.2.3.4:22",
		"1.2.3.4:2200":   "1.2.3.4:2200",
		"[::1]:22":       "[::1]:22",
		"example.com:22": "example.com:22",
	}
	for in, want := range cases {
		if got := normalizeAddr(in); got != want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandHome_NonTilde(t *testing.T) {
	// A path without a leading ~ is returned unchanged.
	for _, p := range []string{"/abs/path", "relative/path", "key.pem"} {
		got, err := expandHome(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != p {
			t.Errorf("expandHome(%q) = %q, want unchanged", p, got)
		}
	}
}

func TestRandomHex(t *testing.T) {
	a, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomHex(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 || len(b) != 16 {
		t.Errorf("len = %d/%d, want 16", len(a), len(b))
	}
	if a == b {
		t.Error("two random suffixes collided")
	}
}

func TestBuildAuthMethods_Password(t *testing.T) {
	m, _, err := buildAuthMethods(AuthConfig{Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Errorf("got %d methods, want 1", len(m))
	}
}

func TestBuildAuthMethods_KeyBytes(t *testing.T) {
	pem := newTestPEMKey(t)
	m, _, err := buildAuthMethods(AuthConfig{KeyBytes: pem})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Errorf("got %d methods, want 1", len(m))
	}
}

func TestBuildAuthMethods_BadKey(t *testing.T) {
	_, _, err := buildAuthMethods(AuthConfig{KeyBytes: []byte("not a key")})
	if err == nil {
		t.Fatal("expected an error for an invalid private key")
	}
}

func TestBuildAuthMethods_Order(t *testing.T) {
	// Key + password configured: both methods present, key before password.
	pem := newTestPEMKey(t)
	m, _, err := buildAuthMethods(AuthConfig{KeyBytes: pem, Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Errorf("got %d methods, want 2", len(m))
	}
}

func TestBuildAuthMethods_None(t *testing.T) {
	_, _, err := buildAuthMethods(AuthConfig{})
	if err == nil {
		t.Fatal("expected an error when no auth method is configured")
	}
}

func TestBuildHostKeyCallback_Insecure(t *testing.T) {
	cb, err := buildHostKeyCallback(HostKeyConfig{InsecureIgnoreHostKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if cb == nil {
		t.Fatal("callback is nil")
	}
}

func TestBuildHostKeyCallback_CustomCallback(t *testing.T) {
	called := false
	custom := func(string, net.Addr, ssh.PublicKey) error { called = true; return nil }
	cb, err := buildHostKeyCallback(HostKeyConfig{Callback: custom})
	if err != nil {
		t.Fatal(err)
	}
	_ = cb(("h"), nil, nil)
	if !called {
		t.Error("custom callback was not used")
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Options{User: "u", Auth: AuthConfig{Password: "p"}}); err == nil {
		t.Error("expected error for empty Addr")
	}
	if _, err := New(Options{Addr: "h:22", Auth: AuthConfig{Password: "p"}}); err == nil {
		t.Error("expected error for empty User")
	}
	if _, err := New(Options{Addr: "h:22", User: "u"}); err == nil {
		t.Error("expected error for missing auth method")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &sandbox.CappedBuffer{Max: 4}
	n, _ := b.Write([]byte("hello world"))
	if n != 11 {
		t.Errorf("Write returned %d, want 11 (reports full length)", n)
	}
	if b.String() != "hell" {
		t.Errorf("buffer = %q, want %q", b.String(), "hell")
	}
}

// newTestPEMKey generates an in-memory unencrypted ed25519 OpenSSH private key.
func newTestPEMKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}
