package store

import (
	"context"
	"strings"
	"testing"
)

// A span keeps the image references its input items carry; reading the span
// lists the attachments beside it, as an entry does, and the payload itself
// is never rewritten. The summary listing has no payload, so no attachments.
func TestTraceSpanListsItsAttachments(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ts := NewTraceStore(db)
	atts := NewAttachmentStore(db)
	id := ids(t)
	a := &Attachment{OwnerID: LocalUserID, Key: "attachments/u/a.png", Mime: "image/png", Size: 10}
	if err := atts.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	gone := NewID() // referenced by the span, but its row is gone
	data := `{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"` + AttachmentSentinelURL(a.ID) + `","detail":"auto"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + AttachmentSentinelURL(a.ID) + `"},{"type":"input_image","image_url":"` + AttachmentSentinelURL(gone) + `"}]}` +
		`],"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a cat"}]}]}`
	if got := TraceAttachmentIDs(data); len(got) != 2 || got[0] != a.ID || got[1] != gone {
		t.Fatalf("TraceAttachmentIDs = %v, want [%s %s], each once, in order", got, a.ID, gone)
	}
	if got := TraceAttachmentIDs(`{"input":[{"role":"user","content":"plain"}]}`); got != nil {
		t.Fatalf("a payload without references = %v, want none", got)
	}

	gen := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-gen", Name: "generation", Detail: "generation", Data: data}
	if err := ts.Insert(ctx, gen); err != nil {
		t.Fatal(err)
	}
	span, err := ts.GetBySpan(ctx, id("s1"), "sp-gen")
	if err != nil {
		t.Fatal(err)
	}
	if len(span.Attachments) != 1 || span.Attachments[0].ID != a.ID || span.Attachments[0].Key != a.Key {
		t.Fatalf("span attachments = %+v, want the one row that exists", span.Attachments)
	}
	if !strings.Contains(span.Data, AttachmentSentinelURL(a.ID)) {
		t.Fatalf("the payload must keep its reference: %s", span.Data)
	}

	full, err := ts.ListBySession(ctx, id("s1"), "", 0)
	if err != nil || len(full) != 1 || len(full[0].Attachments) != 1 {
		t.Fatalf("full listing = %+v (%v), want the attachment beside the span", full, err)
	}
	summary, err := ts.ListSummaryBySession(ctx, id("s1"), "", 0)
	if err != nil || len(summary) != 1 || summary[0].Attachments != nil {
		t.Fatalf("summary listing = %+v (%v), want no attachments without a payload", summary, err)
	}
}
