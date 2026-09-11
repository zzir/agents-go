package store_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A list field stores as JSON text and reads back as the list it was: a row
// written before the arrays (a JSON string in the column, "" for unset) reads
// the same, and an explicit empty selection stays distinct from none.
func TestAgentConfigListFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "lists", Model: "m",
		Tools: store.StringList{"srv-1"}, Skills: store.StringList{}, Approval: store.ApprovalGroup{ApproveTools: store.StringList{"*"}}}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, ac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0] != "srv-1" || len(got.Approval.ApproveTools) != 1 {
		t.Fatalf("lists did not round-trip: tools=%v approve=%v", got.Tools, got.Approval.ApproveTools)
	}
	if got.Skills == nil || len(got.Skills) != 0 {
		t.Fatalf("an explicit empty skills selection must stay [], got %#v", got.Skills)
	}
	if got.Handoffs != nil {
		t.Fatalf("an unset list must read nil, got %#v", got.Handoffs)
	}

	// A row as an earlier build wrote it: the columns hold JSON text or "".
	if _, err := db.NewUpdate().Model((*store.AgentConfig)(nil)).
		Set("tools = ?", `["a","b"]`).Set("skills = ?", "").Set("handoffs = ?", `[]`).
		Where("id = ?", ac.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, ac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 || got.Tools[1] != "b" {
		t.Fatalf("legacy tools text did not read back: %v", got.Tools)
	}
	if got.Skills != nil {
		t.Fatalf(`a legacy "" skills column must read nil (every skill), got %#v`, got.Skills)
	}
	if got.Handoffs == nil || len(got.Handoffs) != 0 {
		t.Fatalf("a legacy [] must read as an empty list, got %#v", got.Handoffs)
	}
}
