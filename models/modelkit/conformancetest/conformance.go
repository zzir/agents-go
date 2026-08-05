// Package conformancetest is the golden test matrix every agents.Model
// adapter in this repository must pass.
//
// It checks the adapter against the runner's consumption contract (spec
// §5.10): which output item types come back, which stream events appear and
// in what order, how usage is accounted, and that every synthesized item
// round-trips into next-turn input. The suite drives the Model interface
// only — each adapter supplies a NewModel hook that returns a Model backed by
// a fake backend speaking that adapter's own wire protocol, primed to answer
// the scenario. The suite cannot know wire formats; translating a TurnSpec
// into wire bytes is the adapter test's half of the bargain, and is itself
// the translation being verified.
package conformancetest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// UsageSpec is the canonical token accounting a scenario's turn must report.
// Input is the TOTAL input count including cached reads and writes (Responses
// semantics); TotalTokens must come out as Input+Output.
type UsageSpec struct {
	Input      int64
	Output     int64
	CachedRead int64
	CacheWrite int64
	Reasoning  int64
}

// ToolCallSpec is one function call the turn must produce.
type ToolCallSpec struct {
	CallID        string
	Name          string
	ArgumentsJSON string
}

// ReasoningSpec is the reasoning item the turn must produce. Encrypted seeds
// the fixture's continuity blob (a thinking signature, encrypted reasoning,
// …). The blob's canonical form is the ADAPTER's to choose — it may wrap or
// prefix the wire value — so the suite asserts presence and byte-identical
// survival into next-turn input, not equality with this seed.
type ReasoningSpec struct {
	Text      string
	Encrypted string
}

// TurnSpec is what the model must answer for a scenario, in canonical terms.
// The adapter's fixture encodes this meaning in its own wire format.
type TurnSpec struct {
	ResponseID string
	Text       string
	Reasoning  *ReasoningSpec
	ToolCalls  []ToolCallSpec
	Truncated  bool
	Usage      UsageSpec
}

// Scenario is one request/turn pair the suite runs, in both blocking and
// streaming mode.
type Scenario struct {
	Name     string
	UserText string
	Tools    []*agents.FunctionTool
	Settings *agents.ModelSettings
	Turn     TurnSpec
}

// Request builds the ModelRequest the suite sends for this scenario.
func (s Scenario) Request() agents.ModelRequest {
	return agents.ModelRequest{
		SystemInstructions: "You are a test fixture.",
		Input:              agents.InputItemsFromText(s.UserText),
		Settings:           s.Settings,
		Tools:              s.Tools,
	}
}

type lookupArgs struct {
	Query string `json:"query"`
}

func lookupTool() *agents.FunctionTool {
	return agents.NewFunctionTool("lookup", "Look something up.",
		func(_ context.Context, _ *agents.ToolContext, _ lookupArgs) (string, error) {
			return "ok", nil
		})
}

// Scenarios returns the golden matrix.
func Scenarios() []Scenario {
	return []Scenario{
		{
			Name:     "simple_text",
			UserText: "What is the capital of Norway?",
			Turn: TurnSpec{
				ResponseID: "resp_text",
				Text:       "The capital of Norway is Oslo.",
				Usage:      UsageSpec{Input: 12, Output: 8},
			},
		},
		{
			Name:     "reasoning",
			UserText: "Think, then answer.",
			Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortLow}},
			Turn: TurnSpec{
				ResponseID: "resp_reasoning",
				Reasoning:  &ReasoningSpec{Text: "Weighing the options.", Encrypted: "sig-continuity-1"},
				Text:       "Done.",
				Usage:      UsageSpec{Input: 20, Output: 30, Reasoning: 16},
			},
		},
		{
			Name:     "tool_call",
			UserText: "Look up the weather.",
			Tools:    []*agents.FunctionTool{lookupTool()},
			Turn: TurnSpec{
				ResponseID: "resp_tool",
				ToolCalls:  []ToolCallSpec{{CallID: "call_1", Name: "lookup", ArgumentsJSON: `{"query":"weather"}`}},
				Usage:      UsageSpec{Input: 25, Output: 12},
			},
		},
		{
			Name:     "parallel_tool_calls",
			UserText: "Look up two things.",
			Tools:    []*agents.FunctionTool{lookupTool()},
			Turn: TurnSpec{
				ResponseID: "resp_parallel",
				ToolCalls: []ToolCallSpec{
					{CallID: "call_1", Name: "lookup", ArgumentsJSON: `{"query":"a"}`},
					{CallID: "call_2", Name: "lookup", ArgumentsJSON: `{"query":"b"}`},
				},
				Usage: UsageSpec{Input: 25, Output: 24},
			},
		},
		{
			Name:     "truncated",
			UserText: "Write a novel.",
			Settings: &agents.ModelSettings{MaxTokens: agents.Ptr(int64(16))},
			Turn: TurnSpec{
				ResponseID: "resp_truncated",
				Text:       "It was a dark and",
				Truncated:  true,
				Usage:      UsageSpec{Input: 10, Output: 16},
			},
		},
		{
			Name:     "cached_usage",
			UserText: "Hello again.",
			Turn: TurnSpec{
				ResponseID: "resp_cached",
				Text:       "Hi.",
				Usage:      UsageSpec{Input: 100, Output: 5, CachedRead: 60, CacheWrite: 20},
			},
		},
	}
}

// Target is the adapter's side of the suite.
type Target struct {
	// NewModel returns a Model backed by a fake backend primed to answer the
	// scenario in the adapter's wire format.
	NewModel func(t *testing.T, s Scenario) agents.Model
}

// Run executes the golden matrix against the target, each scenario in blocking
// and streaming mode.
func Run(t *testing.T, target Target) {
	t.Helper()
	for _, s := range Scenarios() {
		t.Run(s.Name+"/blocking", func(t *testing.T) {
			model := target.NewModel(t, s)
			resp, err := model.GetResponse(context.Background(), s.Request())
			if err != nil {
				t.Fatalf("GetResponse: %v", err)
			}
			assertResponse(t, s.Turn, resp)
		})
		t.Run(s.Name+"/streaming", func(t *testing.T) {
			model := target.NewModel(t, s)
			resp := consumeStream(t, model, s)
			assertResponse(t, s.Turn, resp)
		})
	}
}

// assertResponse checks a final ModelResponse against the turn spec.
func assertResponse(t *testing.T, spec TurnSpec, resp *agents.ModelResponse) {
	t.Helper()
	if resp.ResponseID != spec.ResponseID {
		t.Errorf("ResponseID = %q, want %q", resp.ResponseID, spec.ResponseID)
	}
	if got := resp.Truncated(); got != spec.Truncated {
		t.Errorf("Truncated() = %v (status=%q reason=%q), want %v",
			got, resp.Status, resp.IncompleteReason, spec.Truncated)
	}

	var text strings.Builder
	var calls []ToolCallSpec
	var reasonings []agents.TResponseOutputItem
	for i, item := range resp.Output {
		if item.RawJSON() == "" {
			t.Fatalf("output item %d has empty RawJSON — it cannot round-trip into next-turn input", i)
		}
		switch item.Type {
		case "message":
			for _, part := range item.AsMessage().Content {
				text.WriteString(part.AsOutputText().Text)
			}
		case "function_call":
			fc := item.AsFunctionCall()
			calls = append(calls, ToolCallSpec{CallID: fc.CallID, Name: fc.Name, ArgumentsJSON: fc.Arguments})
		case "reasoning":
			reasonings = append(reasonings, item)
		default:
			t.Errorf("output item %d has type %q — the runner only models message/reasoning/function_call", i, item.Type)
		}
	}

	if text.String() != spec.Text {
		t.Errorf("message text = %q, want %q", text.String(), spec.Text)
	}
	assertToolCalls(t, spec.ToolCalls, calls)
	assertReasoning(t, spec.Reasoning, reasonings)
	assertRoundTrip(t, spec, resp.Output)
	assertUsage(t, spec.Usage, resp.Usage)
}

func assertToolCalls(t *testing.T, want, got []ToolCallSpec) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("function calls = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.CallID != w.CallID || g.Name != w.Name {
			t.Errorf("call %d = %s/%s, want %s/%s", i, g.CallID, g.Name, w.CallID, w.Name)
		}
		if !jsonEqual(g.ArgumentsJSON, w.ArgumentsJSON) {
			t.Errorf("call %d arguments = %s, want %s", i, g.ArgumentsJSON, w.ArgumentsJSON)
		}
	}
}

func assertReasoning(t *testing.T, spec *ReasoningSpec, items []agents.TResponseOutputItem) {
	t.Helper()
	if spec == nil {
		if len(items) != 0 {
			t.Errorf("unexpected reasoning items: %d", len(items))
		}
		return
	}
	if len(items) != 1 {
		t.Fatalf("reasoning items = %d, want 1", len(items))
	}
	r := items[0].AsReasoning()
	var text strings.Builder
	for _, s := range r.Summary {
		text.WriteString(s.Text)
	}
	for _, c := range r.Content {
		text.WriteString(c.Text)
	}
	if text.String() != spec.Text {
		t.Errorf("reasoning text = %q, want %q", text.String(), spec.Text)
	}
	if spec.Encrypted != "" && r.EncryptedContent == "" {
		t.Error("reasoning item lost its continuity blob: encrypted_content is empty")
	}
}

// assertRoundTrip converts the output into next-turn input — the conversion
// every multi-turn run performs — and checks nothing load-bearing is lost.
func assertRoundTrip(t *testing.T, spec TurnSpec, output []agents.TResponseOutputItem) {
	t.Helper()
	inputs, err := agents.OutputToInput(output)
	if err != nil {
		t.Fatalf("OutputToInput: %v", err)
	}
	for i, in := range inputs {
		raw, err := agents.MarshalInputItem(in)
		if err != nil {
			t.Fatalf("marshaling input item %d: %v", i, err)
		}
		if _, err := agents.UnmarshalInputItem(raw); err != nil {
			t.Fatalf("unmarshaling input item %d: %v", i, err)
		}
	}
	if spec.Reasoning != nil && spec.Reasoning.Encrypted != "" {
		// Survival is the contract: whatever canonical form the adapter chose
		// for the blob must come back byte-identical from next-turn input —
		// that is what lets the backend resume its reasoning.
		var respEnc string
		for _, item := range output {
			if item.Type == "reasoning" {
				respEnc = item.AsReasoning().EncryptedContent
			}
		}
		var found bool
		for _, in := range inputs {
			raw, _ := agents.MarshalInputItem(in)
			var probe struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			_ = json.Unmarshal(raw, &probe)
			if probe.Type == "reasoning" && respEnc != "" && probe.EncryptedContent == respEnc {
				found = true
			}
		}
		if !found {
			t.Error("encrypted reasoning content did not survive the round trip into next-turn input")
		}
	}
}

func assertUsage(t *testing.T, spec UsageSpec, u *agents.Usage) {
	t.Helper()
	if u == nil {
		t.Fatal("Usage is nil")
	}
	if u.Requests != 1 {
		t.Errorf("Requests = %d, want 1", u.Requests)
	}
	wantTotal := spec.Input + spec.Output
	if u.InputTokens != spec.Input || u.OutputTokens != spec.Output || u.TotalTokens != wantTotal {
		t.Errorf("tokens = in %d / out %d / total %d, want %d / %d / %d",
			u.InputTokens, u.OutputTokens, u.TotalTokens, spec.Input, spec.Output, wantTotal)
	}
	if u.InputTokensDetails.CachedTokens != spec.CachedRead {
		t.Errorf("CachedTokens = %d, want %d", u.InputTokensDetails.CachedTokens, spec.CachedRead)
	}
	if u.InputTokensDetails.CacheWriteTokens != spec.CacheWrite {
		t.Errorf("CacheWriteTokens = %d, want %d", u.InputTokensDetails.CacheWriteTokens, spec.CacheWrite)
	}
	if u.OutputTokensDetails.ReasoningTokens != spec.Reasoning {
		t.Errorf("ReasoningTokens = %d, want %d", u.OutputTokensDetails.ReasoningTokens, spec.Reasoning)
	}
}

// allowedEventTypes is the closed set a synthesized stream may emit. The
// runner switches on a few of these and forwards the rest to consumers; an
// event name outside the Responses vocabulary would leak the backend's own
// protocol into every downstream consumer.
var allowedEventTypes = map[string]bool{
	"response.created":                       true,
	"response.in_progress":                   true,
	"response.output_item.added":             true,
	"response.output_item.done":              true,
	"response.content_part.added":            true,
	"response.content_part.done":             true,
	"response.output_text.delta":             true,
	"response.output_text.done":              true,
	"response.reasoning_text.delta":          true,
	"response.reasoning_text.done":           true,
	"response.reasoning_summary_part.added":  true,
	"response.reasoning_summary_part.done":   true,
	"response.reasoning_summary_text.delta":  true,
	"response.reasoning_summary_text.done":   true,
	"response.function_call_arguments.delta": true,
	"response.function_call_arguments.done":  true,
	"response.completed":                     true,
	"response.incomplete":                    true,
}

// consumeStream drains StreamResponse, enforcing the stream contract, and
// assembles the final ModelResponse the way the runner does.
func consumeStream(t *testing.T, model agents.Model, s Scenario) *agents.ModelResponse {
	t.Helper()
	var (
		events       []*agents.TResponseStreamEvent
		final        *agents.ModelResponse
		terminalSeen bool
		doneItems    []agents.TResponseOutputItem
		doneItemIDs  = map[string]bool{}
		textDeltas   strings.Builder
		reasonDeltas strings.Builder
		argsDeltas   = map[string]*strings.Builder{}
	)
	for event, err := range model.StreamResponse(context.Background(), s.Request()) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if event == nil {
			continue
		}
		if terminalSeen {
			t.Fatalf("event %q after the terminal event", event.Type)
		}
		if !allowedEventTypes[event.Type] {
			t.Fatalf("event type %q is outside the Responses stream vocabulary", event.Type)
		}
		if len(events) == 0 && event.Type != "response.created" {
			t.Errorf("first event = %q, want response.created", event.Type)
		}
		events = append(events, event)
		switch event.Type {
		case "response.output_item.done":
			done := event.AsResponseOutputItemDone()
			doneItems = append(doneItems, done.Item)
			if done.Item.ID != "" {
				doneItemIDs[done.Item.ID] = true
			}
		case "response.output_text.delta":
			d := event.AsResponseOutputTextDelta()
			if doneItemIDs[d.ItemID] {
				t.Errorf("output_text.delta for item %q after its output_item.done", d.ItemID)
			}
			textDeltas.WriteString(d.Delta)
		case "response.reasoning_text.delta":
			d := event.AsResponseReasoningTextDelta()
			if doneItemIDs[d.ItemID] {
				t.Errorf("reasoning_text.delta for item %q after its output_item.done", d.ItemID)
			}
			reasonDeltas.WriteString(d.Delta)
		case "response.reasoning_summary_text.delta":
			d := event.AsResponseReasoningSummaryTextDelta()
			if doneItemIDs[d.ItemID] {
				t.Errorf("reasoning_summary_text.delta for item %q after its output_item.done", d.ItemID)
			}
			reasonDeltas.WriteString(d.Delta)
		case "response.function_call_arguments.delta":
			d := event.AsResponseFunctionCallArgumentsDelta()
			if doneItemIDs[d.ItemID] {
				t.Errorf("function_call_arguments.delta for item %q after its output_item.done", d.ItemID)
			}
			b := argsDeltas[d.ItemID]
			if b == nil {
				b = &strings.Builder{}
				argsDeltas[d.ItemID] = b
			}
			b.WriteString(d.Delta)
		case "response.completed":
			terminalSeen = true
			completed := event.AsResponseCompleted()
			final = &agents.ModelResponse{
				Output:     completed.Response.Output,
				Usage:      usageFromFinal(&completed.Response),
				ResponseID: completed.Response.ID,
				Status:     string(completed.Response.Status),
			}
		case "response.incomplete":
			terminalSeen = true
			inc := event.AsResponseIncomplete()
			final = &agents.ModelResponse{
				Output:           inc.Response.Output,
				Usage:            usageFromFinal(&inc.Response),
				ResponseID:       inc.Response.ID,
				Status:           string(inc.Response.Status),
				IncompleteReason: inc.Response.IncompleteDetails.Reason,
			}
		}
	}
	if !terminalSeen || final == nil {
		t.Fatal("stream ended without response.completed / response.incomplete")
	}
	if len(final.Output) == 0 {
		final.Output = doneItems
	}
	if s.Turn.Text != "" && textDeltas.String() != s.Turn.Text {
		t.Errorf("output_text deltas concatenate to %q, want %q — streaming consumers render these", textDeltas.String(), s.Turn.Text)
	}
	if s.Turn.Reasoning != nil && s.Turn.Reasoning.Text != "" && reasonDeltas.String() != s.Turn.Reasoning.Text {
		t.Errorf("reasoning deltas concatenate to %q, want %q", reasonDeltas.String(), s.Turn.Reasoning.Text)
	}
	// Argument deltas are optional in the contract, but when a stream emits
	// them they must reassemble to the finished call's arguments — incremental
	// JSON assembly is exactly where a translating adapter goes wrong.
	for _, item := range doneItems {
		if item.Type != "function_call" {
			continue
		}
		fc := item.AsFunctionCall()
		if b, ok := argsDeltas[item.ID]; ok && !jsonEqual(b.String(), fc.Arguments) {
			t.Errorf("argument deltas for %q concatenate to %s, want %s", fc.Name, b.String(), fc.Arguments)
		}
	}
	assertDoneItemsMatchFinal(t, doneItems, final.Output)
	return final
}

// assertDoneItemsMatchFinal checks that the per-item done events and the
// terminal event's output agree: the runner uses the terminal output but falls
// back to accumulated done items when a backend omits it, so the two must be
// interchangeable.
func assertDoneItemsMatchFinal(t *testing.T, done, final []agents.TResponseOutputItem) {
	t.Helper()
	if len(done) != len(final) {
		t.Fatalf("output_item.done count %d != terminal output count %d", len(done), len(final))
	}
	for i := range done {
		var a, b any
		if err := json.Unmarshal([]byte(done[i].RawJSON()), &a); err != nil {
			t.Fatalf("done item %d: %v", i, err)
		}
		if err := json.Unmarshal([]byte(final[i].RawJSON()), &b); err != nil {
			t.Fatalf("final item %d: %v", i, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("item %d differs between output_item.done and the terminal output:\n%s\n%s",
				i, done[i].RawJSON(), final[i].RawJSON())
		}
	}
}

// usageFromFinal mirrors the runner's own extraction from a streamed terminal
// response (stream_run.go), so the suite asserts what the run would record.
func usageFromFinal(resp *responses.Response) *agents.Usage {
	if !resp.JSON.Usage.Valid() {
		return agents.NewUsage()
	}
	u := resp.Usage
	return &agents.Usage{
		Requests:     1,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
		InputTokensDetails: agents.InputTokensDetails{
			CachedTokens:     u.InputTokensDetails.CachedTokens,
			CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
		},
		OutputTokensDetails: agents.OutputTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
	}
}

func jsonEqual(a, b string) bool {
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
