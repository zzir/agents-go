package store

import (
	"context"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// imageEntry is a user message carrying the named attachments.
func imageEntry(t *testing.T, ids ...string) session.Entry {
	t.Helper()
	parts := `{"type":"input_text","text":"look"}`
	for _, id := range ids {
		parts += `,{"type":"input_image","image_url":"` + AttachmentSentinelURL(id) + `"}`
	}
	return rawEntryFrom(t, `{"type":"message","role":"user","content":[`+parts+`]}`, agents.Source{Type: agents.SourceUser})
}

// Deleting a session unbinds the attachments only its entries referenced, so
// the reaper collects them; one a fork (another session's entry) still shows
// stays bound until that session goes too.
func TestSessionDeleteUnbindsTheAttachmentsOnlyItReferenced(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	atts := NewAttachmentStore(db)

	own := &Attachment{OwnerID: LocalUserID, Key: "att/own.png", Mime: "image/png", Size: 1}
	shared := &Attachment{OwnerID: LocalUserID, Key: "att/shared.png", Mime: "image/png", Size: 1}
	for _, a := range []*Attachment{own, shared} {
		if err := atts.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := atts.MarkBound(ctx, []string{own.ID, shared.ID}); err != nil {
		t.Fatal(err)
	}
	src := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatal(err)
	}
	s := storeFor(t, db, src.ID)
	seed(t, s, userEntry(t, "hi"), imageEntry(t, own.ID, shared.ID), imageEntry(t, shared.ID))
	// The fork copies the entries, shared reference included, up to the cut.
	all, err := s.GetEntries(ctx, refOf(t, db, src.ID), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	fork := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "fork"}
	if _, err := s.ForkSession(ctx, fork, refOf(t, db, src.ID), all[2].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewUpdate().Model((*entryRow)(nil)).
		Set("entry = REPLACE(entry, ?, ?)", AttachmentSentinelURL(own.ID), "https://elsewhere/x.png").
		Where("session_id = ?", fork.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := atts.Get(ctx, own.ID); got == nil || got.Bound {
		t.Fatalf("own = %+v, want unbound: nothing references it any more", got)
	}
	if got, _ := atts.Get(ctx, shared.ID); got == nil || !got.Bound {
		t.Fatalf("shared = %+v, want still bound: the fork's entry shows it", got)
	}
	orphans, err := atts.ListUnboundBefore(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != own.ID {
		t.Fatalf("reaper would collect %+v, want the own attachment alone", orphans)
	}

	if err := sessions.Delete(ctx, fork.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := atts.Get(ctx, shared.ID); got == nil || got.Bound {
		t.Fatalf("shared = %+v, want unbound once the last reference is gone", got)
	}
}
