package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func mustItem(t *testing.T, raw string) agents.InputItem {
	t.Helper()
	it, err := agents.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return it
}

func TestSanitizeConversationItem_StripsTopLevelID(t *testing.T) {
	item := mustItem(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}],"id":"msg_1"}`)
	clean, keep, err := sanitizeConversationItem(item)
	if err != nil || !keep {
		t.Fatalf("keep=%v err=%v", keep, err)
	}
	b, _ := json.Marshal(clean)
	if strings.Contains(string(b), `"id"`) {
		t.Fatalf("id should be stripped: %s", b)
	}
}

func TestSanitizeConversationItem_DropsReasoningWithoutID(t *testing.T) {
	item := mustItem(t, `{"type":"reasoning","summary":[]}`)
	_, keep, err := sanitizeConversationItem(item)
	if err != nil {
		t.Fatal(err)
	}
	if keep {
		t.Fatal("reasoning lacking id and encrypted_content must be unpersistable")
	}
}

func TestSanitizeConversationItem_KeepsReasoningWithEncryptedContent(t *testing.T) {
	item := mustItem(t, `{"type":"reasoning","summary":[],"encrypted_content":"abc"}`)
	_, keep, err := sanitizeConversationItem(item)
	if err != nil {
		t.Fatal(err)
	}
	if !keep {
		t.Fatal("reasoning with encrypted_content must be persistable")
	}
}

// TestConversationsSession_AddSanitizes exercises AddItems end-to-end: a
// reasoning item that lacks both an id and encrypted content is dropped, so only
// the message reaches the (fake) Conversations API.
func TestConversationsSession_AddSanitizes(t *testing.T) {
	ctx := t.Context()
	s, fake := newTestSession(t)

	items := []agents.InputItem{
		mustItem(t, `{"type":"reasoning","summary":[]}`),
		mustItem(t, `{"role":"user","content":"hello"}`),
	}
	if err := agents.NewSession(s).AppendItems(ctx, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	got := len(fake.items)
	fake.mu.Unlock()
	if got != 1 {
		t.Fatalf("expected 1 persisted item (reasoning dropped), got %d", got)
	}
}
