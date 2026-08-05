package agentstest

import (
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// ResponseBuilder scripts a [FakeModel] one turn at a time.
//
// Items added after construction land in the first turn; [ResponseBuilder.NewTurn]
// starts the next one. Each turn becomes one model response, so a script with
// two turns drives a run that calls the model twice.
//
//	model := agentstest.NewResponseBuilder().
//	    Reasoning("the user wants weather").
//	    FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
//	    NewTurn().
//	    Text("it is sunny").
//	    Build()
type ResponseBuilder struct {
	turns []Turn
	// itemSeq numbers generated item ids across the whole script so every item
	// has a distinct id, as a real provider would produce.
	itemSeq int
}

// NewResponseBuilder starts a script with one empty turn.
func NewResponseBuilder() *ResponseBuilder {
	return &ResponseBuilder{turns: []Turn{{}}}
}

// NewTurn ends the current turn and starts the next.
func (b *ResponseBuilder) NewTurn() *ResponseBuilder {
	b.turns = append(b.turns, Turn{})
	return b
}

func (b *ResponseBuilder) cur() *Turn { return &b.turns[len(b.turns)-1] }

func (b *ResponseBuilder) nextID(prefix string) string {
	b.itemSeq++
	return fmt.Sprintf("%s_%d", prefix, b.itemSeq)
}

// Item appends a pre-built output item to the current turn.
func (b *ResponseBuilder) Item(item agents.OutputItem) *ResponseBuilder {
	t := b.cur()
	t.Items = append(t.Items, item)
	return b
}

// Text appends an assistant message.
func (b *ResponseBuilder) Text(s string) *ResponseBuilder {
	return b.Item(MessageItem(b.nextID("msg"), s))
}

// Reasoning appends a reasoning item.
func (b *ResponseBuilder) Reasoning(s string) *ResponseBuilder {
	return b.Item(ReasoningItem(b.nextID("rs"), s))
}

// FunctionCall appends a function tool call. argsJSON is the raw arguments
// string; pass invalid JSON to exercise the argument-parsing error path.
func (b *ResponseBuilder) FunctionCall(name, callID, argsJSON string) *ResponseBuilder {
	return b.Item(FunctionCallItem(b.nextID("fc"), name, callID, argsJSON))
}

// Refusal appends an assistant refusal, which fails the run with an
// [agents.ModelRefusalError] unless a recovery handler is configured.
func (b *ResponseBuilder) Refusal(s string) *ResponseBuilder {
	return b.Item(RefusalItem(b.nextID("msg"), s))
}

// Raw appends an item from its wire JSON — including types the SDK does not
// recognize. Malformed JSON panics; see [RawItem].
func (b *ResponseBuilder) Raw(rawJSON string) *ResponseBuilder {
	return b.Item(RawItem(rawJSON))
}

// Usage overrides the token usage reported for the current turn.
func (b *ResponseBuilder) Usage(u agents.RequestUsage) *ResponseBuilder {
	b.cur().Usage = u
	return b
}

// ResponseID overrides the response identifier reported for the current turn.
// Set it when a test chains calls with previous_response_id.
func (b *ResponseBuilder) ResponseID(id string) *ResponseBuilder {
	b.cur().ResponseID = id
	return b
}

// Fail makes the current turn return err instead of a response, exercising the
// model-call failure path. Any items already added to the turn are ignored.
func (b *ResponseBuilder) Fail(err error) *ResponseBuilder {
	b.cur().Err = err
	return b
}

// Build returns the scripted model. A trailing empty turn (from a NewTurn with
// nothing after it) is dropped so the script length matches the intent.
func (b *ResponseBuilder) Build() *FakeModel {
	turns := b.turns
	for len(turns) > 0 {
		last := turns[len(turns)-1]
		if len(last.Items) == 0 && last.Err == nil {
			turns = turns[:len(turns)-1]
			continue
		}
		break
	}
	return NewFakeModel(turns...)
}
