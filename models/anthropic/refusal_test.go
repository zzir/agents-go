package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
)

// refusalJSON is a blocking Messages response whose stop_reason is refusal —
// the API reports refusal OUT-OF-BAND, with the refusal text as an ordinary
// text block.
const refusalJSON = `{
	"id": "msg_r", "type": "message", "role": "assistant", "model": "claude-test",
	"content": [{"type": "text", "text": "I cannot help with that."}],
	"stop_reason": "refusal",
	"usage": {"input_tokens": 10, "output_tokens": 5}
}`

func refusalProvider(t *testing.T) agents.ModelProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, refusalJSON)
	}))
	t.Cleanup(srv.Close)
	return NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
}

// A refusal stop must reach the runner as a canonical refusal part: as plain
// output_text the run would "succeed" with the refusal as its answer and
// error_handlers.model_refusal could never fire.
func TestRunSyncRefusalSurfacesModelRefusalError(t *testing.T) {
	agent := &agents.Agent{Name: "a", Model: "claude-test"}
	_, err := agents.RunSync(context.Background(), agent, "do the thing", agents.RunOptions{
		Model: agents.ModelOptions{Provider: refusalProvider(t)},
	})
	if err == nil {
		t.Fatal("a refusal must fail the run, not complete it")
	}
	var refErr *agents.ModelRefusalError
	if !errors.As(err, &refErr) {
		t.Fatalf("want ModelRefusalError, got %T: %v", err, err)
	}
	if refErr.Refusal != "I cannot help with that." {
		t.Errorf("refusal text = %q", refErr.Refusal)
	}
}

// The model_refusal error handler recovers the run with its fallback output.
func TestRunSyncRefusalRecoversViaErrorHandler(t *testing.T) {
	agent := &agents.Agent{Name: "a", Model: "claude-test"}
	res, err := agents.RunSync(context.Background(), agent, "do the thing", agents.RunOptions{
		Model: agents.ModelOptions{Provider: refusalProvider(t)},
		Exec: agents.ExecOptions{ErrorHandlers: agents.RunErrorHandlers{
			ModelRefusal: func(context.Context, agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
				return &agents.RunErrorHandlerResult{FinalOutput: "declined — please rephrase"}, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("handler must recover the run: %v", err)
	}
	if got := res.FinalOutputString(); got != "declined — please rephrase" {
		t.Errorf("fallback output = %q", got)
	}
}

// The streamed path: stop_reason arrives in message_delta, and the terminal
// rebuild must still deliver the refusal part to the runner.
func TestStreamedRunRefusalSurfacesModelRefusalError(t *testing.T) {
	model := sseModel(t, sseEvents(
		`{"type":"message_start","message":{"id":"msg_r","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I cannot help with that."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	))
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	stream, _ := agents.Run(context.Background(), agent, "do the thing", agents.RunOptions{})
	var runErr error
	for _, err := range stream {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("a streamed refusal must fail the run")
	}
	if _, ok := errors.AsType[*agents.ModelRefusalError](runErr); !ok {
		t.Fatalf("want ModelRefusalError, got %T: %v", runErr, runErr)
	}
}

// A refused response may carry partially generated tool_use blocks; they must
// be dropped at conversion — the runner executes tool calls before it ever
// looks for a refusal, and a refused response's actions must not run.
func TestRunSyncRefusalDropsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_r", "type": "message", "role": "assistant", "model": "claude-test",
			"content": [
				{"type": "text", "text": "I cannot help with that."},
				{"type": "tool_use", "id": "toolu_1", "name": "rm_rf", "input": {}}
			],
			"stop_reason": "refusal",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`)
	}))
	t.Cleanup(srv.Close)

	ran := false
	tool := agents.NewTool("rm_rf", "dangerous",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			ran = true
			return "gone", nil
		})
	agent := &agents.Agent{Name: "a", Model: "claude-test", Tools: []*agents.Tool{tool}}
	_, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Model: agents.ModelOptions{Provider: NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))},
	})
	if _, ok := errors.AsType[*agents.ModelRefusalError](err); !ok {
		t.Fatalf("want ModelRefusalError, got %T: %v", err, err)
	}
	if ran {
		t.Fatal("a refused response's tool call must not execute")
	}
}

// A refusal with empty content still surfaces a non-empty refusal — from
// stop_details' explanation, or the fixed line when even that is absent.
func TestRunSyncRefusalEmptyContentUsesExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_r", "type": "message", "role": "assistant", "model": "claude-test",
			"content": [],
			"stop_reason": "refusal",
			"stop_details": {"type": "refusal", "category": "general_harms", "explanation": "Declined per policy."},
			"usage": {"input_tokens": 10, "output_tokens": 0}
		}`)
	}))
	t.Cleanup(srv.Close)

	agent := &agents.Agent{Name: "a", Model: "claude-test"}
	_, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Model: agents.ModelOptions{Provider: NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))},
	})
	var refErr *agents.ModelRefusalError
	if !errors.As(err, &refErr) {
		t.Fatalf("want ModelRefusalError, got %T: %v", err, err)
	}
	if refErr.Refusal != "Declined per policy." {
		t.Errorf("refusal should carry stop_details.explanation, got %q", refErr.Refusal)
	}
}
