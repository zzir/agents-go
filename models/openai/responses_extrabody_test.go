package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
)

// TestRequestOptions_ExtraBodyLeadingColonKey pins that an ExtraBody key with a
// leading ':' reaches the request body verbatim. sjson (used by WithJSONSet)
// treats a leading ':' as a force-string-key marker and strips it, so without
// escaping, {":k": "v"} would be silently renamed to {"k": "v"}.
func TestRequestOptions_ExtraBodyLeadingColonKey(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp_1",
			"created_at": 0,
			"object":     "response",
			"model":      "gpt-4o",
			"status":     "completed",
			"output":     []any{},
			"usage":      map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		})
	}))
	t.Cleanup(srv.Close)

	client := oai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	m := NewResponsesModel("gpt-4o", client.Responses)

	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{
			ExtraBody: map[string]any{":k": "v"},
		},
	}
	if _, err := m.Respond(t.Context(), req); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("unmarshal request body %q: %v", gotBody, err)
	}
	if v, ok := body[":k"]; !ok || v != "v" {
		t.Errorf("request body[\":k\"] = %v (present=%v), want \"v\"; full body: %v", v, ok, body)
	}
	// Regression guard: the leading colon must not be stripped into a "k" key.
	if _, bad := body["k"]; bad {
		t.Errorf("leading ':' was stripped: found key \"k\" in request body %v", body)
	}
}
