package agentstest

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// Turn is one scripted model response: the items it produces, its usage, and
// optionally a transport error instead of a response.
type Turn struct {
	Items []agents.OutputItem
	Usage agents.RequestUsage
	// Err, when non-nil, makes the model return this error instead of a
	// response — the model-call failure path.
	Err error
	// ResponseID is reported as the response identifier. Defaults to a
	// generated value.
	ResponseID string
}

// defaultTurnUsage is what a turn reports when the script does not say.
// Non-zero so tests that assert usage accumulation see movement.
var defaultTurnUsage = agents.RequestUsage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}

// zeroTurnUsage asks completedEvent for a genuinely empty usage block, which
// the plain zero value cannot express — that one means "unset, substitute the
// default". Used for an exhausted script, which bills nothing.
var zeroTurnUsage = agents.RequestUsage{TotalTokens: -1}

// FakeModel is a scripted [agents.Model]. Each model call consumes the next
// turn; once the script is exhausted every further call returns an empty
// response, which the runner treats as a final output of "".
//
// It is safe for concurrent use: nested agent-as-tool runs may share one.
type FakeModel struct {
	mu       sync.Mutex
	turns    []Turn
	idx      int
	requests []agents.ModelRequest

	// StreamTextDeltas emits per-character output_text deltas for message items
	// during streaming. Off by default: most tests only care about the
	// assembled response, and deltas make transcripts noisy.
	StreamTextDeltas bool
}

// NewFakeModel returns a model that replays the given turns in order.
// Prefer [NewResponseBuilder] unless you are assembling turns programmatically.
func NewFakeModel(turns ...Turn) *FakeModel { return &FakeModel{turns: turns} }

// TextModel is the shorthand for a model that answers with plain text, one
// turn per string.
func TextModel(texts ...string) *FakeModel {
	b := NewResponseBuilder()
	for i, t := range texts {
		if i > 0 {
			b.NewTurn()
		}
		b.Text(t)
	}
	return b.Build()
}

// next pops the next scripted turn, recording the request.
func (m *FakeModel) next(req agents.ModelRequest) (Turn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.idx >= len(m.turns) {
		return Turn{}, false
	}
	t := m.turns[m.idx]
	m.idx++
	return t, true
}

// Calls reports how many model calls have been made.
func (m *FakeModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// Requests returns every request the model received, in order.
func (m *FakeModel) Requests() []agents.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]agents.ModelRequest(nil), m.requests...)
}

// LastRequest returns the most recent request. It panics when the model has
// not been called — asserting on a request that never happened is a test bug,
// not a condition to handle.
func (m *FakeModel) LastRequest() agents.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		panic("agentstest: LastRequest called before the model was invoked")
	}
	return m.requests[len(m.requests)-1]
}

// Remaining reports how many scripted turns have not been consumed. A test
// that expected the whole script to run can assert this is zero.
func (m *FakeModel) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return max(0, len(m.turns)-m.idx)
}

func (t Turn) response(seq int) *agents.ModelResponse {
	u := t.Usage
	if u == (agents.RequestUsage{}) {
		u = defaultTurnUsage
	}
	if u == zeroTurnUsage {
		u = agents.RequestUsage{}
	}
	id := t.ResponseID
	if id == "" {
		id = fmt.Sprintf("resp_%d", seq)
	}
	return &agents.ModelResponse{
		Output:     t.Items,
		ResponseID: id,
		Usage: &agents.Usage{
			Requests:            1,
			InputTokens:         u.InputTokens,
			OutputTokens:        u.OutputTokens,
			TotalTokens:         u.TotalTokens,
			InputTokensDetails:  u.InputTokensDetails,
			OutputTokensDetails: u.OutputTokensDetails,
			RequestUsageEntries: []agents.RequestUsage{u},
		},
	}
}

// Respond implements [agents.Model].
func (m *FakeModel) Respond(_ context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	turn, ok := m.next(req)
	if !ok {
		return &agents.ModelResponse{Usage: agents.NewUsage()}, nil
	}
	if turn.Err != nil {
		return nil, turn.Err
	}
	return turn.response(m.Calls()), nil
}

// StreamResponse implements [agents.Model]. It emits one
// response.output_item.done per item followed by response.completed, plus
// output_text deltas when StreamTextDeltas is set.
func (m *FakeModel) StreamResponse(_ context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		turn, ok := m.next(req)
		if !ok {
			// An exhausted script bills nothing, on BOTH paths: the zero
			// RequestUsage would be substituted with defaultTurnUsage, so a
			// usage assertion passed under RunSync and failed under Run.
			ev := completedEvent(nil, "resp_empty", zeroTurnUsage)
			yield(&ev, nil)
			return
		}
		if turn.Err != nil {
			yield(nil, turn.Err)
			return
		}
		seq := 0
		emit := func(ev agents.ResponseStreamEvent) bool {
			seq++
			return yield(&ev, nil)
		}
		for i, item := range turn.Items {
			if m.StreamTextDeltas {
				for _, ev := range textDeltaEvents(i, item) {
					if !emit(ev) {
						return
					}
				}
			}
			if !emit(outputItemDoneEvent(i, item)) {
				return
			}
		}
		resp := turn.response(m.Calls())
		u := turn.Usage
		if u == (agents.RequestUsage{}) {
			u = defaultTurnUsage
		}
		emit(completedEvent(turn.Items, resp.ResponseID, u))
	}
}

var _ agents.Model = (*FakeModel)(nil)

// --- stream event construction ---------------------------------------------
//
// Events are synthesized by the modelkit builders (the same ones adapters
// use), wrapped in this package's panic contract.

func mustEvent(ev agents.ResponseStreamEvent, err error) agents.ResponseStreamEvent {
	if err != nil {
		panic(fmt.Sprintf("agentstest: %v", err))
	}
	return ev
}

func outputItemDoneEvent(index int, item agents.OutputItem) agents.ResponseStreamEvent {
	return mustEvent(modelkit.OutputItemDoneEvent(index, item))
}

func completedEvent(items []agents.OutputItem, respID string, u agents.RequestUsage) agents.ResponseStreamEvent {
	if u == (agents.RequestUsage{}) {
		u = defaultTurnUsage
	}
	if u == zeroTurnUsage {
		u = agents.RequestUsage{}
	}
	return mustEvent(modelkit.CompletedEvent(modelkit.FinalResponse{
		ID:     respID,
		Output: items,
		Usage: modelkit.ResponseUsage{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			TotalTokens:      u.TotalTokens,
			CachedTokens:     u.InputTokensDetails.CachedTokens,
			CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
			ReasoningTokens:  u.OutputTokensDetails.ReasoningTokens,
		},
	}))
}

// textDeltaEvents produces per-rune output_text deltas for a message item so
// streaming consumers see incremental text. Non-message items produce none.
func textDeltaEvents(index int, item agents.OutputItem) []agents.ResponseStreamEvent {
	if item.Type != "message" {
		return nil
	}
	var text strings.Builder
	for _, part := range item.AsMessage().Content {
		if t := part.AsOutputText(); t.Text != "" {
			text.WriteString(t.Text)
		}
	}
	if text.Len() == 0 {
		return nil
	}
	itemID := item.ID
	events := make([]agents.ResponseStreamEvent, 0, len([]rune(text.String()))+1)
	for _, r := range text.String() {
		events = append(events, mustEvent(modelkit.OutputTextDeltaEvent(itemID, index, string(r))))
	}
	return events
}
