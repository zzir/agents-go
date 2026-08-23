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
	for _, k := range []string{settings.KeyOpenAIAPIKey, settings.KeyAnthropicAPIKey, settings.KeyBraveAPIKey} {
		if !settings.IsSecret(k) {
			t.Errorf("%s must be masked", k)
		}
	}
	for _, k := range []string{settings.KeyProxyURL, settings.KeySystemPrompt, "no_such_key"} {
		if settings.IsSecret(k) {
			t.Errorf("%s must not be masked", k)
		}
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

// trace_include_sensitive_data is the one tri-state: unset must stay nil so
// the SDK can read OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA. Defaulting it
// here would take that escape hatch away.
func TestTraceSensitiveStaysTriState(t *testing.T) {
	r, w := newReader(t)
	ctx := t.Context()
	if got := r.BoolPtr(ctx, settings.KeyTraceIncludeSensitiveData); got != nil {
		t.Fatalf("unset must be nil, got %v", *got)
	}
	if err := w.Set(ctx, settings.KeyTraceIncludeSensitiveData, "false"); err != nil {
		t.Fatal(err)
	}
	got := r.BoolPtr(ctx, settings.KeyTraceIncludeSensitiveData)
	if got == nil || *got {
		t.Fatalf("explicit false must come through as a pointer, got %v", got)
	}
	if err := w.Set(ctx, settings.KeyTraceIncludeSensitiveData, "garbage"); err != nil {
		t.Fatal(err)
	}
	if got := r.BoolPtr(ctx, settings.KeyTraceIncludeSensitiveData); got != nil {
		t.Fatalf("an unusable value must defer to the SDK, got %v", *got)
	}
}
