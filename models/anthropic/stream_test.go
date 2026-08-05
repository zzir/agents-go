package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
)

// sseModel returns a MessagesModel whose fake backend replies with the given
// raw SSE body for streaming calls.
func sseModel(t *testing.T, sse string) agents.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	t.Cleanup(srv.Close)
	provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	model, err := provider.GetModel("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// sseEvents wraps each JSON payload in an SSE frame whose event name mirrors
// the payload's leading "type" field, as the real API does.
func sseEvents(events ...string) string {
	var b strings.Builder
	for _, data := range events {
		typ := data[strings.Index(data, `"type":"`)+len(`"type":"`):]
		typ = typ[:strings.Index(typ, `"`)]
		b.WriteString("event: " + typ + "\ndata: " + data + "\n\n")
	}
	return b.String()
}

func TestStreamContextWindowExceededSurfacesOverflowError(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"model_context_window_exceeded","stop_sequence":null},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	))
	var streamErr error
	for _, err := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("expected an error for stop_reason model_context_window_exceeded")
	}
	if !agents.DetectContextOverflow(streamErr) {
		t.Fatalf("the overflow detector must recognize it: %v", streamErr)
	}
}

func TestStreamRefusalStopReasonCompletes(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I can't help with that."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":6}}`,
		`{"type":"message_stop"}`,
	))
	var sawCompleted bool
	for event, err := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("a refusal stop must still complete the response — the text is the answer")
	}
}

// The protocol only orders content_block_start events by index; deltas and
// stops for still-open blocks may interleave. The terminal output must come
// back in INDEX order regardless — a stop-ordered history would replay with
// thinking after text, which the API rejects.
func TestStreamOutOfOrderBlockStops(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"late thought"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-x"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9,"output_tokens_details":{"thinking_tokens":4}}}`,
		`{"type":"message_stop"}`,
	))
	var final *agents.ModelResponse
	for event, err := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.completed" {
			completed := event.AsResponseCompleted()
			final = &agents.ModelResponse{Output: completed.Response.Output}
			if got := completed.Response.Usage.OutputTokensDetails.ReasoningTokens; got != 4 {
				t.Errorf("streamed ReasoningTokens = %d, want 4 (from message_delta)", got)
			}
		}
	}
	if final == nil {
		t.Fatal("no terminal event")
	}
	if len(final.Output) != 2 || final.Output[0].Type != "reasoning" || final.Output[1].Type != "message" {
		types := make([]string, len(final.Output))
		for i, it := range final.Output {
			types[i] = it.Type
		}
		t.Fatalf("terminal output order = %v, want [reasoning message] (index order)", types)
	}
	if enc := final.Output[0].AsReasoning().EncryptedContent; enc != signaturePrefix+"sig-x" {
		t.Errorf("encrypted_content = %q, want prefixed signature", enc)
	}
}

// A stream that ends cleanly without message_stop was severed at an event
// boundary (an idle gateway timeout sending a clean FIN): the SSE layer sees a
// normal EOF, but the response is cut off. It must surface as a retryable
// truncation, not a silent end the runner then reports unretryably.
func TestStreamEndsWithoutMessageStopIsRetryableTruncation(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
	))
	var streamErr error
	for _, err := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want an io.ErrUnexpectedEOF wrap", streamErr)
	}
	if !RetryableError(streamErr) {
		t.Fatal("a truncated stream must classify as retryable")
	}
}

func TestStreamAbandonedByConsumerStops(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	))
	count := 0
	for _, err := range model.StreamResponse(context.Background(), agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
	}) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatalf("consumed %d events, want 2", count)
	}
}
