package session

import "cmp"

// SourceType says who produced an item. The zero value is the model, which is
// where most items come from.
//
// It replaces a marker the SDK used to smuggle into the item itself: a fake
// response id ("__fake_id__") stamped on synthesized items, which every
// consumer then had to know about and string-compare. Provenance is not an id.
type SourceType string

const (
	// SourceModel is a direct model output — the zero value.
	SourceModel SourceType = ""
	// SourceUser is input handed to the run from outside.
	SourceUser SourceType = "user"
	// SourceTool is the result of executing a tool.
	SourceTool SourceType = "tool"
	// SourceHandoff is the synthesized acknowledgement of a handoff.
	SourceHandoff SourceType = "handoff"
	// SourceErrorHandler is a fallback synthesized by RunOptions.Exec.ErrorHandlers.
	SourceErrorHandler SourceType = "error_handler"
	// SourceCompaction is a summary written by session-history compaction.
	SourceCompaction SourceType = "compaction"
	// SourceGuardrail is content substituted by a guardrail's Replace decision.
	SourceGuardrail SourceType = "guardrail"
)

// Source records an item's provenance.
type Source struct {
	Type SourceType `json:"type,omitzero"`
	// ID names the specific producer when there can be several of a kind — a
	// guardrail's name, an error handler's kind. Empty when the type is enough.
	ID string `json:"id,omitzero"`
}

// IsExternal reports whether the item came from outside the SDK — the model or
// the caller — as opposed to something the runner synthesized.
//
// The distinction matters wherever the SDK must not feed itself: a context
// provider that injects content and later reads history back must not re-ingest
// its own injections, and a compaction summary must not be summarized again.
func (s Source) IsExternal() bool {
	return s.Type == SourceModel || s.Type == SourceUser
}

// String renders the source for logs and traces.
func (s Source) String() string {
	t := string(s.Type)
	t = cmp.Or(t, "model")
	if s.ID == "" {
		return t
	}
	return t + ":" + s.ID
}
