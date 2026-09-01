package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunInput is what a person's message carries into a run: the text, and the
// image attachments uploaded beforehand. Attachments ride only THIS path —
// task spawns, workflow steps and injections stay text-only.
type RunInput struct {
	Text          string
	AttachmentIDs []string
}

// TextInput is the text-only RunInput every internal caller passes.
func TextInput(text string) RunInput { return RunInput{Text: text} }

// items builds the run's input items. Text-only keeps the plain-string shape
// the SDK normalizes itself; a message with attachments becomes one user
// message whose content list holds the text and an input_image part per
// attachment — carrying the SENTINEL url, which is what gets persisted; the
// model boundary resolves it (attachment_hydrate.go). The JSON below is
// static and well-formed by construction, so a failure is unreachable; it
// degrades to the text alone rather than growing an error path every caller
// would have to invent a policy for.
func (in RunInput) items() []agents.InputItem {
	if len(in.AttachmentIDs) == 0 {
		return agents.InputItemsFromText(in.Text)
	}
	type part struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL string `json:"image_url,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}
	parts := make([]part, 0, len(in.AttachmentIDs)+1)
	if in.Text != "" {
		parts = append(parts, part{Type: "input_text", Text: in.Text})
	}
	for _, id := range in.AttachmentIDs {
		parts = append(parts, part{Type: "input_image", ImageURL: store.AttachmentSentinelURL(id), Detail: "auto"})
	}
	raw, err := json.Marshal(map[string]any{"type": "message", "role": "user", "content": parts})
	if err != nil {
		return agents.InputItemsFromText(in.Text)
	}
	item, err := session.UnmarshalInputItem(raw)
	if err != nil {
		return agents.InputItemsFromText(in.Text)
	}
	return []agents.InputItem{item}
}

// validateAttachments checks a run's attachment_ids the way the pre-flight
// needs them checked: bounded in count, every id present, and owned by the
// session's owner (a foreign id reads the same as a missing one — ownership
// is not an oracle for existence). It returns the metadata for the announce
// and bind steps.
func (r *Runner) validateAttachments(ctx context.Context, ownerID string, ids []string) (map[string]store.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if r.Deps.Attachments == nil {
		return nil, fmt.Errorf("attachments are not supported on this server")
	}
	if len(ids) > attachments.MaxPerMessage {
		return nil, fmt.Errorf("a message carries at most %d images, got %d", attachments.MaxPerMessage, len(ids))
	}
	meta, err := r.Deps.Attachments.MetaBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		a, ok := meta[id]
		if !ok || (ownerID != "" && a.OwnerID != ownerID) {
			return nil, fmt.Errorf("attachment %s not found", id)
		}
	}
	return meta, nil
}
