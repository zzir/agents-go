package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The guards that keep parallel title generation from misfiring: it must not
// rename a session that already has a title, nor make a model call without a
// user message or a provider.
func TestMaybeGenerateTitleGuards(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	runner := NewRunner(ctx, db, &AgentDeps{Sessions: sessions})

	// Already-titled session: no-op even with a user message (name guard runs
	// first, before touching a model).
	titled := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "Existing Title"}
	if err := sessions.Create(ctx, titled); err != nil {
		t.Fatalf("create titled session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, titled.ID, "gpt-test", "hello there", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, titled.ID); got.Name != "Existing Title" {
		t.Errorf("titled session renamed to %q", got.Name)
	}

	// New Session but no user input: no model call, no rename.
	empty := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "New Session"}
	if err := sessions.Create(ctx, empty); err != nil {
		t.Fatalf("create empty session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, empty.ID, "gpt-test", "", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, empty.ID); got.Name != "New Session" {
		t.Errorf("empty New Session renamed to %q", got.Name)
	}

	// New Session with input but no provider: still bails before any model call.
	noProv := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "New Session"}
	if err := sessions.Create(ctx, noProv); err != nil {
		t.Fatalf("create no-provider session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, noProv.ID, "gpt-test", "a question", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, noProv.ID); got.Name != "New Session" {
		t.Errorf("no-provider New Session renamed to %q", got.Name)
	}
}

// The fallback name is the first line of the message, clipped like every
// machine-made name (invariant 78).
func TestFallbackTitleClipsTheFirstLine(t *testing.T) {
	if got := fallbackTitle("  hello there  \nand a second line"); got != "hello there" {
		t.Fatalf("fallbackTitle = %q, want the first line", got)
	}
	long := "Research the inter-session context handoff design and write up the findings"
	got := fallbackTitle(long)
	if r := []rune(got); len(r) != store.AutoNameMax || r[len(r)-1] != '…' {
		t.Fatalf("fallbackTitle(long) = %q (%d runes), want %d ending in an ellipsis", got, len(r), store.AutoNameMax)
	}
}

// A title is shown as text, so markdown wrapping the WHOLE of it is stripped
// before the clip (invariant 78); marks inside it are the person's own words.
func TestPlainTitleStripsMarkdown(t *testing.T) {
	for in, want := range map[string]string{
		// Wrapped: what a model returns despite being told not to.
		"**Fix build**":             "Fix build",
		"# Plan":                    "Plan",
		"### Release notes":         "Release notes",
		"`go test`":                 "go test",
		`"Quoted"`:                  "Quoted",
		"'Quoted'":                  "Quoted",
		"“Quoted”":                  "Quoted",
		"[docs](https://x)":         "docs",
		"> Summary of the thread":   "Summary of the thread",
		"- Fix the flaky test":      "Fix the flaky test",
		"1. Getting started":        "Getting started",
		"**\"Deploy plan\"**":       "Deploy plan",
		"***Bold and italic***":     "Bold and italic",
		"_Draft_":                   "Draft",
		"**2 * 3**":                 "2 * 3",
		"  Spread \n over\tlines  ": "Spread over lines",
		// Not wrapped: inner marks stay as written.
		"fix user_id and order_id":   "fix user_id and order_id",
		"rename *.go to *.txt":       "rename *.go to *.txt",
		"2 * 3 * 4":                  "2 * 3 * 4",
		"__init__.py":                "__init__.py",
		"_private_var_":              "_private_var_",
		"*args and **kwargs":         "*args and **kwargs",
		"**Fix** the **build**":      "**Fix** the **build**",
		`"a" and "b"`:                `"a" and "b"`,
		"`a` and `b`":                "`a` and `b`",
		`It's "fine"`:                `It's "fine"`,
		"#123 is broken":             "#123 is broken",
		"-5 degrees":                 "-5 degrees",
		"[docs](https://x) and more": "[docs](https://x) and more",
		"see [docs](https://x)":      "see [docs](https://x)",
		"C# basics":                  "C# basics",
		// Marks around nothing are no title: the caller falls back.
		"**": "",
		`""`: "",
	} {
		if got := plainTitle(in); got != want {
			t.Errorf("plainTitle(%q) = %q, want %q", in, got, want)
		}
	}
	// The fallback is the person's first line, through the same rule.
	if got := fallbackTitle("**Ship it**\nthe rest"); got != "Ship it" {
		t.Errorf("fallbackTitle = %q, want the first line without its markdown", got)
	}
	if got := fallbackTitle("fix user_id in __init__.py"); got != "fix user_id in __init__.py" {
		t.Errorf("fallbackTitle = %q, want the person's words untouched", got)
	}
}
