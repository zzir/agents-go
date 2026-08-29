package settings_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newReader(t *testing.T) (*settings.Reader, *store.SettingStore) {
	t.Helper()
	s := store.NewSettingStore(testdb.New(t))
	return settings.NewReader(s), s
}

// Every def must be usable: a key the panel renders, a kind the panel knows,
// and a default that survives its own validation. A def whose default is
// rejected by Validate would be a setting nobody can type back in by hand.
func TestDefsAreSelfConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range settings.Defs() {
		if seen[d.Key] {
			t.Fatalf("duplicate key %q", d.Key)
		}
		seen[d.Key] = true
		if d.Label == "" || d.Group == "" {
			t.Errorf("%s: needs a label and a group", d.Key)
		}
		switch d.Kind {
		case settings.KindString, settings.KindText, settings.KindSecret, settings.KindInt, settings.KindBool:
		default:
			t.Errorf("%s: unknown kind %q", d.Key, d.Kind)
		}
		if err := settings.Validate(d.Key, d.Default); err != nil {
			t.Errorf("%s: its own default %q does not validate: %v", d.Key, d.Default, err)
		}
	}
}

// A secret is a kind, not a hand-kept list — that is what stops a new
// credential setting from shipping unmasked.
func TestIsSecretFollowsTheKind(t *testing.T) {
	for _, d := range settings.Defs() {
		if got := settings.IsSecret(d.Key); got != (d.Kind == settings.KindSecret) {
			t.Errorf("IsSecret(%s) = %v, want the kind (%s) to decide", d.Key, got, d.Kind)
		}
	}
	if settings.IsSecret("no_such_key") {
		t.Error("an unknown key must not be masked")
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
		wantErr          bool
	}{
		{"unknown key", "not_a_setting", "x", true},
		{"empty is always a reset", settings.KeyTraceSpanDataKB, "", false},
		{"int accepts a number", settings.KeyTraceSpanDataKB, "4096", false},
		{"int rejects words", settings.KeyTraceSpanDataKB, "lots", true},
		{"int rejects below min", settings.KeyTraceSpanDataKB, "0", true},
		{"int rejects above max", settings.KeyMaxTerminalsPerSandbox, "99", true},
		{"int accepts within max", settings.KeyMaxTerminalsPerSandbox, "8", false},
		{"ttl accepts zero", settings.KeyApprovalTTLMinutes, "0", false},
		{"bool accepts false", settings.KeyTraceIncludeSensitiveData, "false", false},
		{"bool rejects maybe", settings.KeyTraceIncludeSensitiveData, "maybe", true},
		{"proxy accepts a URL", settings.KeyProxyURL, "socks5://127.0.0.1:1080", false},
		{"proxy rejects a bare host", settings.KeyProxyURL, "127.0.0.1:7890", true},
		{"free text takes anything", settings.KeySystemPrompt, "be terse\nand kind", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := settings.Validate(tc.key, tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q, %q) = %v, wantErr %v", tc.key, tc.value, err, tc.wantErr)
			}
		})
	}
	if err := settings.Validate("not_a_setting", "x"); !errors.Is(err, settings.ErrUnknownKey) {
		t.Fatalf("an unknown key must be identifiable: %v", err)
	}
}

// A nil Reader is the unconfigured server: every read yields the registered
// default rather than a zero value or a panic.
func TestNilReaderYieldsDefaults(t *testing.T) {
	var r *settings.Reader
	ctx := context.Background()
	if got := r.Int(ctx, settings.KeyApprovalTTLMinutes); got != 1440 {
		t.Errorf("approval TTL = %d, want the 1440 default", got)
	}
	if got := r.Int(ctx, settings.KeyTraceSpanDataKB); got != 8192 {
		t.Errorf("span cap = %d, want the 8192 default", got)
	}
	if got := r.String(ctx, settings.KeyProxyURL); got != "" {
		t.Errorf("proxy = %q, want empty", got)
	}
}

func TestReaderReadsStoredValues(t *testing.T) {
	r, w := newReader(t)
	ctx := t.Context()
	// Operators type these by hand; a stray space must not fall back.
	if err := w.Set(ctx, settings.KeyApprovalTTLMinutes, " 15 "); err != nil {
		t.Fatal(err)
	}
	if got := r.Int(ctx, settings.KeyApprovalTTLMinutes); got != 15 {
		t.Errorf("TTL = %d, want 15", got)
	}
	if err := w.Set(ctx, settings.KeyTraceIncludeSensitiveData, "true"); err != nil {
		t.Fatal(err)
	}
	if !r.Bool(ctx, settings.KeyTraceIncludeSensitiveData) {
		t.Error("a stored bool must win over the default")
	}
	// A value that predates validation must not take the feature down.
	if err := w.Set(ctx, settings.KeyTraceSpanDataKB, "garbage"); err != nil {
		t.Fatal(err)
	}
	if got := r.Int(ctx, settings.KeyTraceSpanDataKB); got != 8192 {
		t.Errorf("unparsable stored value = %d, want the default 8192", got)
	}
}

// trace_include_sensitive_data resolves like every other bool: the registered
// default (true) when unset or unusable, the stored value otherwise. The
// server always passes the result explicitly — the SDK's environment variable
// is not consulted here.
func TestTraceSensitiveResolvesDefault(t *testing.T) {
	r, w := newReader(t)
	ctx := t.Context()
	if !r.Bool(ctx, settings.KeyTraceIncludeSensitiveData) {
		t.Fatal("unset must resolve to the default true")
	}
	if err := w.Set(ctx, settings.KeyTraceIncludeSensitiveData, "false"); err != nil {
		t.Fatal(err)
	}
	if r.Bool(ctx, settings.KeyTraceIncludeSensitiveData) {
		t.Fatal("explicit false must come through")
	}
	if err := w.Set(ctx, settings.KeyTraceIncludeSensitiveData, "garbage"); err != nil {
		t.Fatal(err)
	}
	if !r.Bool(ctx, settings.KeyTraceIncludeSensitiveData) {
		t.Fatal("an unusable value must fall back to the default true")
	}
}

// A fresh http.Transport is a fresh connection pool, and ProxyClient is called
// per agent build, per compaction, per MCP transport and per token refresh —
// so with a proxy configured nothing was reused. The client is pooled, keyed
// by the URL, which is also the whole invalidation story.
func TestProxyClientIsPooledPerURL(t *testing.T) {
	r, s := newReader(t)
	ctx := context.Background()

	if c := r.ProxyClient(ctx); c != nil {
		t.Fatal("no proxy set: want nil")
	}
	if err := s.Set(ctx, settings.KeyProxyURL, "http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}

	first := r.ProxyClient(ctx)
	if first == nil {
		t.Fatal("proxy set: want a client")
	}
	if again := r.ProxyClient(ctx); again != first {
		t.Error("the same proxy URL built a second client")
	}

	if err := s.Set(ctx, settings.KeyProxyURL, "socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	changed := r.ProxyClient(ctx)
	if changed == nil || changed == first {
		t.Error("an edited proxy URL must produce a different client")
	}

	var nilReader *settings.Reader
	if c := nilReader.ProxyClient(ctx); c != nil {
		t.Error("a nil Reader must proxy nothing")
	}
}
