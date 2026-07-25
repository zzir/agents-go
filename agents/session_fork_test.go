package agents

import (
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestForkSession_FullCopy(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()
	dst := NewInMemorySession()

	items := []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("hello", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfMessage("hi", responses.EasyInputMessageRoleAssistant),
		responses.ResponseInputItemParamOfMessage("how are you", responses.EasyInputMessageRoleUser),
	}
	if err := src.AppendItems(ctx, items, Source{}); err != nil {
		t.Fatal(err)
	}

	if err := ForkSession(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	got, _ := dst.ContextItems(ctx, Cursor{})
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}

	srcItems, _ := src.ContextItems(ctx, Cursor{})
	if len(srcItems) != 3 {
		t.Fatal("source was modified")
	}
}

func TestForkSession_EmptySource(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()
	dst := NewInMemorySession()

	// Pre-populate dst to verify it gets cleared.
	_ = dst.AppendItems(ctx, []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("old", responses.EasyInputMessageRoleUser),
	}, Source{})

	if err := ForkSession(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	got, _ := dst.ContextItems(ctx, Cursor{})
	if len(got) != 0 {
		t.Fatalf("got %d items, want 0 (dst should be cleared)", len(got))
	}
}

func TestForkSessionAt(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()

	items := []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("a", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfMessage("b", responses.EasyInputMessageRoleAssistant),
		responses.ResponseInputItemParamOfMessage("c", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfMessage("d", responses.EasyInputMessageRoleAssistant),
	}
	_ = src.AppendItems(ctx, items, Source{})

	all, err := src.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		at   string
		want int
	}{
		{"first two", all[1].ID, 2},
		{"just the root", all[0].ID, 1},
		{"the whole branch", all[3].ID, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := NewInMemorySession()
			if err := ForkSessionAt(ctx, src, dst, tt.at); err != nil {
				t.Fatal(err)
			}
			got, _ := dst.ContextItems(ctx, Cursor{})
			if len(got) != tt.want {
				t.Fatalf("got %d items, want %d", len(got), tt.want)
			}
		})
	}

	// An entry that is not on the source's history is an error, not a silent
	// empty fork — the caller passed something wrong and should hear about it.
	if err := ForkSessionAt(ctx, src, NewInMemorySession(), "no-such-entry"); err == nil {
		t.Error("forking at an unknown entry should fail")
	}
}

func TestForkSessionAt_ClearsDst(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()
	dst := NewInMemorySession()

	_ = src.AppendItems(ctx, []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("a", responses.EasyInputMessageRoleUser),
	}, Source{})
	_ = dst.AppendItems(ctx, []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("old1", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfMessage("old2", responses.EasyInputMessageRoleUser),
	}, Source{})

	srcEntries, err := src.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ForkSessionAt(ctx, src, dst, srcEntries[0].ID); err != nil {
		t.Fatal(err)
	}

	got, _ := dst.ContextItems(ctx, Cursor{})
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (dst should contain only forked items)", len(got))
	}
}
