package agents

import (
	"testing"

	"github.com/openai/openai-go/v3/packages/param"
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
	_ = NewSession(dst).AppendItems(ctx, []TResponseInputItem{
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

	tests := []struct {
		name string
		n    int
		want int
	}{
		{"first two", 2, 2},
		{"zero", 0, 0},
		{"negative", -1, 0},
		{"beyond length", 10, 4},
		{"exact length", 4, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := NewInMemorySession()
			if err := ForkSessionAt(ctx, src, dst, tt.n); err != nil {
				t.Fatal(err)
			}
			got, _ := dst.ContextItems(ctx, Cursor{})
			if len(got) != tt.want {
				t.Fatalf("got %d items, want %d", len(got), tt.want)
			}
		})
	}
}

func TestForkSessionAt_ClearsDst(t *testing.T) {
	ctx := t.Context()
	src := NewInMemorySession()
	dst := NewInMemorySession()

	_ = NewSession(src).AppendItems(ctx, []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("a", responses.EasyInputMessageRoleUser),
	}, Source{})
	_ = NewSession(dst).AppendItems(ctx, []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage("old1", responses.EasyInputMessageRoleUser),
		responses.ResponseInputItemParamOfMessage("old2", responses.EasyInputMessageRoleUser),
	}, Source{})

	if err := ForkSessionAt(ctx, src, dst, 1); err != nil {
		t.Fatal(err)
	}

	got, _ := dst.ContextItems(ctx, Cursor{})
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1 (dst should contain only forked items)", len(got))
	}
}

func TestIndexOfItemID(t *testing.T) {
	msgItem := TResponseInputItem{
		OfOutputMessage: &responses.ResponseOutputMessageParam{
			ID:     "msg_001",
			Status: responses.ResponseOutputMessageStatusCompleted,
		},
	}
	fcItem := TResponseInputItem{
		OfFunctionCall: &responses.ResponseFunctionToolCallParam{
			ID:        param.NewOpt("fc_001"),
			CallID:    "call_1",
			Name:      "get_weather",
			Arguments: "{}",
		},
	}
	fcoItem := TResponseInputItem{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			ID:     param.NewOpt("fco_001"),
			CallID: "call_1",
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: param.NewOpt("sunny"),
			},
		},
	}
	userItem := responses.ResponseInputItemParamOfMessage("hello", responses.EasyInputMessageRoleUser)

	items := []TResponseInputItem{userItem, msgItem, fcItem, fcoItem}

	tests := []struct {
		id      string
		wantIdx int
		wantOK  bool
	}{
		{"msg_001", 1, true},
		{"fc_001", 2, true},
		{"fco_001", 3, true},
		{"nonexistent", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			idx, ok := IndexOfItemID(items, tt.id)
			if idx != tt.wantIdx || ok != tt.wantOK {
				t.Fatalf("IndexOfItemID(%q) = (%d, %v), want (%d, %v)", tt.id, idx, ok, tt.wantIdx, tt.wantOK)
			}
		})
	}
}

func TestIndexOfItemID_EmptyItems(t *testing.T) {
	idx, ok := IndexOfItemID(nil, "msg_001")
	if ok || idx != -1 {
		t.Fatalf("got (%d, %v), want (-1, false) for nil items", idx, ok)
	}
}
