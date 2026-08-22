package store

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// The panel reports what the session sends NOW: a second run's snapshot
// replaces the first rather than accumulating.
func TestContextProfileSaveReplaces(t *testing.T) {
	ctx := context.Background()
	s := NewContextProfileStore(newTestDB(t))
	id := ids(t)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	must(s.Save(ctx, id("s1"), PromptProfile{
		MemoryChars: 100,
		Tools:       []ToolBucket{{Source: ToolSourceSandbox, Count: 2, Chars: 300}},
	}))
	must(s.Save(ctx, id("s1"), PromptProfile{
		MemoryChars: 900,
		Tools:       []ToolBucket{{Source: ToolSourceSandbox, Count: 6, Chars: 1200}},
	}))

	got, err := s.Get(ctx, id("s1"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.MemoryChars != 900 || len(got.Tools) != 1 || got.Tools[0].Count != 6 {
		t.Fatalf("second run's snapshot should stand alone, got %+v", got)
	}
}

// A session no run has built yet has no profile, which is absence — not an error
// and not an empty profile the panel would draw as zeros.
func TestContextProfileMissingIsNil(t *testing.T) {
	got, err := NewContextProfileStore(newTestDB(t)).Get(context.Background(), NewID())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil for a session with no profile, got %+v", got)
	}
}

// ToolChars counts the whole definition a request carries, not just the name:
// a fat argument schema is exactly what the panel exists to surface.
func TestToolCharsCountsSchema(t *testing.T) {
	bare := &agents.Tool{Name: "ping"}
	full := &agents.Tool{
		Name:             "ping",
		Description:      "check a host",
		ParamsJSONSchema: map[string]any{"type": "object", "properties": map[string]any{"host": map[string]any{"type": "string"}}},
	}
	if n := ToolChars(bare); n != len("ping") {
		t.Fatalf("bare tool: want %d, got %d", len("ping"), n)
	}
	if ToolChars(full) <= ToolChars(bare)+len("check a host") {
		t.Fatalf("schema not counted: %d vs %d", ToolChars(full), ToolChars(bare))
	}
}
