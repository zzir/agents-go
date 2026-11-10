package agentstest

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// Turn is one scripted model response: the items it produces, its usage, and
// optionally an error instead of a response.
type Turn struct {
	Items []agents.OutputItem
	Usage agents.RequestUsage
	// Err, when non-nil, is returned instead of a response.
	Err error
	// ResponseID is reported as the response identifier; empty means generated.
	ResponseID string
}

// defaultTurnUsage is what a turn reports when the script does not say; non-zero
// so usage-accumulation assertions see movement.
var defaultTurnUsage = agents.RequestUsage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}

// zeroTurnUsage marks a genuinely empty usage block, which the zero value cannot
// express (it means "substitute the default"); an exhausted script bills nothing.
var zeroTurnUsage = agents.RequestUsage{TotalTokens: -1}

// FakeModel is a scripted [agents.Model]: each call consumes the next turn, and
// an exhausted script answers an empty response (a final output of ""). Safe
// for concurrent use, so nested agent-as-tool runs may share one.
type FakeModel struct {
	mu       sync.Mutex
	turns    []Turn
	idx      int
	requests []agents.ModelRequest
}

// NewFakeModel returns a model that replays the given turns in order; prefer
// [NewResponseBuilder] unless assembling turns programmatically.
func NewFakeModel(turns ...Turn) *FakeModel { return &FakeModel{turns: turns} }

// TextModel returns a model that answers with plain text, one turn per string.
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

// LastRequest returns the most recent request; it panics when the model has not
// been called.
func (m *FakeModel) LastRequest() agents.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		panic("agentstest: LastRequest called before the model was invoked")
	}
	return m.requests[len(m.requests)-1]
}

// Remaining reports how many scripted turns have not been consumed.
func (m *FakeModel) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return max(0, len(m.turns)-m.idx)
}

// resolveUsage is the usage a turn bills: the default when the script says
// nothing, nothing for zeroTurnUsage.
func resolveUsage(u agents.RequestUsage) agents.RequestUsage {
	switch u {
	case agents.RequestUsage{}:
		return defaultTurnUsage
	case zeroTurnUsage:
		return agents.RequestUsage{}
	}
	return u
}

func (t Turn) response(seq int) *agents.ModelResponse {
	u := resolveUsage(t.Usage)
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

// StreamResponse implements [agents.Model]: one response.output_item.done per
// item, then response.completed.
func (m *FakeModel) StreamResponse(_ context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		turn, ok := m.next(req)
		if !ok {
			// zeroTurnUsage, not the zero value: that one is substituted with
			// defaultTurnUsage.
			ev := completedEvent(nil, "resp_empty", zeroTurnUsage)
			yield(&ev, nil)
			return
		}
		if turn.Err != nil {
			yield(nil, turn.Err)
			return
		}
		for i, item := range turn.Items {
			ev := outputItemDoneEvent(i, item)
			if !yield(&ev, nil) {
				return
			}
		}
		ev := completedEvent(turn.Items, turn.response(m.Calls()).ResponseID, turn.Usage)
		yield(&ev, nil)
	}
}

var _ agents.Model = (*FakeModel)(nil)

// Stream events come from the modelkit builders, under the package's panic contract.

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
	u = resolveUsage(u)
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
