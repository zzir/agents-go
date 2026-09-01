package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func itemJSON(t *testing.T, item agents.InputItem) string {
	t.Helper()
	b, err := session.MarshalInputItem(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The message builder: text plus attachments become ONE user message whose
// image parts carry the sentinel, never a bucket URL.
func TestRunInputItems(t *testing.T) {
	items := RunInput{Text: "look", AttachmentIDs: []string{"a1", "a2"}}.items()
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	j := itemJSON(t, items[0])
	for _, want := range []string{`"input_text"`, `"look"`, `"input_image"`, store.AttachmentSentinelURL("a1"), store.AttachmentSentinelURL("a2"), `"detail":"auto"`} {
		if !strings.Contains(j, want) {
			t.Errorf("message %s missing %s", j, want)
		}
	}

	// Image-only: no empty text part.
	j = itemJSON(t, RunInput{AttachmentIDs: []string{"a1"}}.items()[0])
	if strings.Contains(j, "input_text") {
		t.Errorf("image-only message grew a text part: %s", j)
	}

	// Text-only keeps the plain message shape.
	if got := (RunInput{Text: "hi"}).items(); len(got) != 1 || session.UserText(got) != "hi" {
		t.Fatalf("text-only items = %v", got)
	}
}

// The model boundary: sentinels resolve to public URLs, missing rows degrade
// to a placeholder, and the caller's items are never mutated.
func TestHydrateAttachments(t *testing.T) {
	db := testdb.New(t)
	atts := store.NewAttachmentStore(db)
	ctx := context.Background()

	a := &store.Attachment{OwnerID: store.LocalUserID, Key: "att/x.png", Mime: "image/png", Size: 3}
	if err := atts.Create(ctx, a); err != nil {
		t.Fatal(err)
	}

	m := hydratingModel{atts: atts, base: func(context.Context) string { return "https://pub.example.com/" }}

	items := RunInput{Text: "see", AttachmentIDs: []string{a.ID, "0197a5c0-0000-4000-8000-000000000000"}}.items()
	before := itemJSON(t, items[0])

	out := m.hydrate(ctx, items)
	got := itemJSON(t, out[0])
	if !strings.Contains(got, "https://pub.example.com/att/x.png") {
		t.Errorf("sentinel not resolved: %s", got)
	}
	if strings.Contains(got, store.AttachmentScheme) {
		t.Errorf("sentinel leaked to the model: %s", got)
	}
	if !strings.Contains(got, "[image unavailable]") {
		t.Errorf("missing row did not degrade: %s", got)
	}
	// Copy-on-write: the stored form still carries the sentinel.
	if after := itemJSON(t, items[0]); after != before {
		t.Errorf("hydrate mutated the caller's item:\n before %s\n after  %s", before, after)
	}

	// Items without sentinels pass through untouched (same backing item).
	plain := agents.InputItemsFromText("no images")
	if out := m.hydrate(ctx, plain); itemJSON(t, out[0]) != itemJSON(t, plain[0]) {
		t.Error("plain item changed")
	}
}

// A tool-produced image with a real URL is not an attachment and must pass
// through hydration byte-identical.
func TestHydrateLeavesRealURLs(t *testing.T) {
	db := testdb.New(t)
	m := hydratingModel{atts: store.NewAttachmentStore(db), base: func(context.Context) string { return "https://pub" }}
	raw := `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://elsewhere.example/x.png"}]}`
	item, err := session.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := m.hydrate(context.Background(), []agents.InputItem{item})
	if got := itemJSON(t, out[0]); !strings.Contains(got, "https://elsewhere.example/x.png") {
		t.Errorf("real URL rewritten: %s", got)
	}
}

// The pre-flight gates, cheapest violation first: a foreign or missing id
// fails before anything runs, and an agent without Vision refuses images with
// an error that names the fix.
func TestRunAttachmentGates(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown attachment id", func(t *testing.T) {
		runner, db := newBareRunner(t)
		cfgID := mkAgent(t, store.NewAgentConfigStore(db), "worker")
		sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "t", AgentConfigID: cfgID}
		if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		res := runner.runStreamed(ctx, store.NewID(), sess.ID, cfgID, "",
			RunInput{Text: "hi", AttachmentIDs: []string{store.NewID()}}, "")
		if res.ErrCode != protocol.CodeConfigError || !strings.Contains(res.ErrMessage, "not found") {
			t.Fatalf("outcome = %q %q, want config error naming the missing attachment", res.ErrCode, res.ErrMessage)
		}
	})

	t.Run("vision off", func(t *testing.T) {
		runner, db := newBareRunner(t)
		cfgID := mkAgent(t, store.NewAgentConfigStore(db), "worker")
		sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "t", AgentConfigID: cfgID}
		if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		a := &store.Attachment{OwnerID: store.LocalUserID, Key: "att/x.png", Mime: "image/png", Size: 1}
		if err := runner.Deps.Attachments.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		res := runner.runStreamed(ctx, store.NewID(), sess.ID, cfgID, "",
			RunInput{Text: "hi", AttachmentIDs: []string{a.ID}}, "")
		if res.ErrCode != protocol.CodeConfigError || !strings.Contains(res.ErrMessage, "Vision") {
			t.Fatalf("outcome = %q %q, want config error naming the Vision flag", res.ErrCode, res.ErrMessage)
		}
		// The gate fired before binding: the upload stays reapable.
		got, err := runner.Deps.Attachments.Get(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Bound {
			t.Error("a refused run must not bind the attachment")
		}
	})
}
