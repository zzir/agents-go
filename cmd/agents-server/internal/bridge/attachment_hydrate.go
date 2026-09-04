package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The decorator below IS the model boundary that resolves attachment sentinels
// — invariant 56; the stored form is never rewritten.

// hydrateAttachments wraps provider so every model it yields resolves
// attachment sentinels before the request leaves; a nil store is a no-op.
func hydrateAttachments(provider agents.ModelProvider, atts *store.AttachmentStore, base func(ctx context.Context) string) agents.ModelProvider {
	if provider == nil || atts == nil {
		return provider
	}
	return hydratingProvider{inner: provider, atts: atts, base: base}
}

type hydratingProvider struct {
	inner agents.ModelProvider
	atts  *store.AttachmentStore
	base  func(ctx context.Context) string
}

func (p hydratingProvider) Model(name string) (agents.Model, error) {
	m, err := p.inner.Model(name)
	if err != nil || m == nil {
		return m, err
	}
	return hydratingModel{inner: m, atts: p.atts, base: p.base}, nil
}

type hydratingModel struct {
	inner agents.Model
	atts  *store.AttachmentStore
	base  func(ctx context.Context) string
}

func (m hydratingModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	req.Input = m.hydrate(ctx, req.Input)
	return m.inner.Respond(ctx, req)
}

func (m hydratingModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	req.Input = m.hydrate(ctx, req.Input)
	return m.inner.StreamResponse(ctx, req)
}

// sentinelMarker fast-rejects items with no sentinel before any JSON work.
var sentinelMarker = []byte(store.AttachmentScheme)

// hydrate returns items with every sentinel replaced by its public URL. Items
// are COPIED, never mutated (run state holds the same slice) — invariant 56.
func (m hydratingModel) hydrate(ctx context.Context, items []agents.InputItem) []agents.InputItem {
	// Pass 1: find the ids, touching only items that mention the scheme.
	raws := make([][]byte, len(items))
	var ids []string
	for i := range items {
		raw, err := session.MarshalInputItem(items[i])
		if err != nil || !bytes.Contains(raw, sentinelMarker) {
			continue
		}
		raws[i] = raw
		ids = append(ids, sentinelIDsIn(raw)...)
	}
	if len(ids) == 0 {
		return items
	}
	meta, err := m.atts.MetaBatch(ctx, ids)
	if err != nil {
		// Leave the sentinels in place: the provider will reject the URL and
		// the run fails loudly, which beats silently dropping the images.
		logging.Ctx(ctx).Error("attachment hydrate: metadata lookup failed", "error", err)
		return items
	}
	base := m.base(ctx)

	out := make([]agents.InputItem, len(items))
	copy(out, items)
	for i, raw := range raws {
		if raw == nil {
			continue
		}
		rewritten, ok := rewriteSentinels(raw, func(id string) (string, bool) {
			a, found := meta[id]
			if !found || base == "" {
				return "", false
			}
			return attachments.PublicURL(base, a.Key), true
		})
		if !ok {
			continue
		}
		item, err := session.UnmarshalInputItem(rewritten)
		if err != nil {
			logging.Ctx(ctx).Error("attachment hydrate: rewritten item does not decode", "error", err)
			continue
		}
		out[i] = item
	}
	return out
}

// sentinelIDsIn extracts attachment ids from an item's wire JSON by walking
// its content parts.
func sentinelIDsIn(raw []byte) []string {
	var probe struct {
		Content []struct {
			ImageURL string `json:"image_url"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	var ids []string
	for _, p := range probe.Content {
		if id := store.AttachmentSentinelID(p.ImageURL); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// rewriteSentinels rewrites each sentinel image_url via resolve; an unresolvable
// one becomes an input_text placeholder. ok=false means nothing changed.
func rewriteSentinels(raw []byte, resolve func(id string) (string, bool)) ([]byte, bool) {
	var item map[string]any
	if json.Unmarshal(raw, &item) != nil {
		return nil, false
	}
	content, _ := item["content"].([]any)
	changed := false
	for i, part := range content {
		pm, _ := part.(map[string]any)
		if pm == nil {
			continue
		}
		u, _ := pm["image_url"].(string)
		id := store.AttachmentSentinelID(u)
		if id == "" {
			continue
		}
		if url, found := resolve(id); found {
			pm["image_url"] = url
		} else {
			content[i] = map[string]any{"type": "input_text", "text": "[image unavailable]"}
		}
		changed = true
	}
	if !changed {
		return nil, false
	}
	b, err := json.Marshal(item)
	if err != nil {
		return nil, false
	}
	return b, true
}
