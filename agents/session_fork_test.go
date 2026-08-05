package agents

import (
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestForkSession_FullCopy(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()
	dst := NewInMemorySession()

	items := []InputItem{
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
	_ = dst.AppendItems(ctx, []InputItem{
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
