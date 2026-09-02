package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The listing the trace panel opens with leaves the payload out and says so;
// one span is fetched whole on demand, 404 when it is not there.
func TestTraceListingSummaryAndSpan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	traces := store.NewTraceStore(db)
	gen := &store.TraceEvent{SessionID: "s1", RunID: "r1", Kind: "span", SpanID: "sp1", Name: "generation", Detail: "generation",
		Data: `{"model":"m","input_tokens":5,"input":[{"role":"user","content":"long"}]}`}
	genData := gen.Data // Insert leaves the row's metadata in Data
	if err := traces.Insert(ctx, gen); err != nil {
		t.Fatal(err)
	}
	h := NewTraceHandler(traces)
	engine := newTestEngine()
	engine.GET("/sessions/:id/traces", h.ListBySession)
	engine.GET("/sessions/:id/traces/:span_id", h.GetBySpan)

	var rows []store.TraceEvent
	w := doJSON(t, engine, http.MethodGet, "/sessions/s1/traces?summary=true", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
		t.Fatalf("summary: %d %s", w.Code, w.Body.String())
	}
	if !rows[0].PayloadOmitted || !traceJSONEqual(rows[0].Data, `{"model":"m","input_tokens":5}`) {
		t.Fatalf("summary row = %+v", rows[0])
	}
	rows = nil // a fresh decode: Unmarshal keeps fields the JSON omits
	w = doJSON(t, engine, http.MethodGet, "/sessions/s1/traces", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 || !traceJSONEqual(rows[0].Data, genData) || rows[0].PayloadOmitted {
		t.Fatalf("full listing: %d %s", w.Code, w.Body.String())
	}
	var one store.TraceEvent
	w = doJSON(t, engine, http.MethodGet, "/sessions/s1/traces/sp1", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &one) != nil || !traceJSONEqual(one.Data, genData) {
		t.Fatalf("span: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodGet, "/sessions/s1/traces/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing span: %d", w.Code)
	}
}

// traceJSONEqual compares two documents as values: a rebuilt payload orders
// its keys.
func traceJSONEqual(a, b string) bool {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return a == b
	}
	return reflect.DeepEqual(va, vb)
}
