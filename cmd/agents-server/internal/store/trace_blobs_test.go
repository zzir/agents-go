package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func blobCount(t *testing.T, ts *TraceStore, sessionID string) int {
	t.Helper()
	n, err := ts.db.NewSelect().Model((*TraceBlob)(nil)).Where("session_id = ?", sessionID).Count(context.Background())
	if err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	return n
}

func spanPayload(t *testing.T, ts *TraceStore, sessionID, spanID string) map[string]any {
	t.Helper()
	ev, err := ts.GetBySpan(context.Background(), sessionID, spanID)
	if err != nil {
		t.Fatalf("GetBySpan %s: %v", spanID, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &m); err != nil {
		t.Fatalf("span %s data is not JSON: %v (%s)", spanID, err, ev.Data)
	}
	return m
}

func backdate(t *testing.T, ts *TraceStore, ev *TraceEvent, at time.Time) {
	t.Helper()
	if _, err := ts.db.NewUpdate().Model((*TraceEvent)(nil)).Set("created_at = ?", at).Where("id = ?", ev.ID).Exec(context.Background()); err != nil {
		t.Fatalf("backdating: %v", err)
	}
}

func item(role, text string) string {
	return fmt.Sprintf(`{"role":%q,"content":%q}`, role, text)
}

// A session's payload elements are stored once: the second generation span of
// a conversation references the first's items rather than carrying them
// again, and a fresh writer learns what the session holds before it writes.
// Large elements are gzipped on the way in, and every span reads back whole.
func TestTracePayloadElementsAreStoredOncePerSession(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	w := ts.NewSpanWriter(id("s1"), 0)

	long := strings.Repeat("the same long history ", 100)
	first := `{"model":"m","system_instructions":"be brief","tools":[{"name":"t"}],"input":[` + item("user", long) + `],"output":[{"type":"message","id":"a"}]}`
	second := `{"model":"m","system_instructions":"be brief","tools":[{"name":"t"}],"input":[` + item("user", long) + `,` + item("assistant", "ok") + `,` + item("user", "next") + `],"output":[{"type":"message","id":"b"}]}`
	e1 := &TraceEvent{RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: first}
	e2 := &TraceEvent{RunID: id("r1"), Kind: "span", SpanID: "g2", Name: "generation", Detail: "generation", Data: second}
	for _, ev := range []*TraceEvent{e1, e2} {
		if err := w.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// long user item, assistant ok, user next, output a, output b, the
	// instructions, the tool: seven distinct elements between the two spans.
	if n := blobCount(t, ts, id("s1")); n != 7 {
		t.Fatalf("blobs = %d, want 7", n)
	}
	if len(e1.Refs) != 4*hashSize || len(e2.Refs) != 6*hashSize {
		t.Fatalf("refs = %d / %d bytes, want 4 and 6 hashes", len(e1.Refs), len(e2.Refs))
	}
	// Layout order is payloadFields order, input first: both spans start with
	// the long user item, and hash it the same.
	if !bytes.Equal(e1.Refs[:hashSize], e2.Refs[:hashSize]) {
		t.Fatal("the shared first item hashed differently across the two spans")
	}
	if e1.Data != `{"model":"m"}` {
		t.Fatalf("row metadata = %q, want the payload fields out", e1.Data)
	}

	var blobs []TraceBlob
	if err := ts.db.NewSelect().Model(&blobs).Where("session_id = ?", id("s1")).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	gzipped, raw := 0, 0
	for _, b := range blobs {
		if bytes.HasPrefix(b.Body, []byte{0x1f, 0x8b}) {
			gzipped++
			if len(b.Body) >= len(long) {
				t.Fatalf("gzipped element is not smaller: %d bytes", len(b.Body))
			}
		} else {
			raw++
			if b.Body[0] != '{' && b.Body[0] != '"' {
				t.Fatalf("raw element is not JSON: %q", b.Body)
			}
		}
	}
	if gzipped != 1 || raw != 6 {
		t.Fatalf("gzipped/raw = %d/%d, want the one long item compressed", gzipped, raw)
	}

	for spanID, want := range map[string]string{"g1": first, "g2": second} {
		got, err := ts.GetBySpan(ctx, id("s1"), spanID)
		if err != nil || !sameJSON(got.Data, want) {
			t.Fatalf("GetBySpan %s = %+v (%v)", spanID, got, err)
		}
	}
	rows, err := ts.ListSummaryBySession(ctx, id("s1"), "", 0)
	if err != nil || len(rows) != 2 || !rows[0].PayloadOmitted || rows[0].Data != `{"model":"m"}` {
		t.Fatalf("summary = %+v (%v)", rows, err)
	}

	// A new run's writer starts by learning the session's elements: writing
	// the same conversation again adds nothing.
	w2 := ts.NewSpanWriter(id("s1"), 0)
	e3 := &TraceEvent{RunID: id("r2"), Kind: "span", SpanID: "g3", Name: "generation", Detail: "generation", Data: second}
	if err := w2.Insert(ctx, e3); err != nil {
		t.Fatal(err)
	}
	if n := blobCount(t, ts, id("s1")); n != 7 {
		t.Fatalf("blobs after a repeat = %d, want still 7", n)
	}
	if got, err := ts.GetBySpan(ctx, id("s1"), "g3"); err != nil || !sameJSON(got.Data, second) {
		t.Fatalf("repeat span = %+v (%v)", got, err)
	}
}

// The element cap replaces the one element over it and nothing else, with a
// marker that names the setting.
func TestTraceElementCapReplacesThatElementAlone(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	w := ts.NewSpanWriter(id("s1"), 100)
	data := `{"input":[` + item("user", strings.Repeat("y", 500)) + `,` + item("user", "small") + `],"system_instructions":"` + strings.Repeat("z", 300) + `","output":[{"type":"message"}]}`
	if err := w.Insert(ctx, &TraceEvent{RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}); err != nil {
		t.Fatal(err)
	}
	m := spanPayload(t, ts, id("s1"), "g1")
	input, _ := m["input"].([]any)
	if len(input) != 2 || input[0] != PayloadCapMarker {
		t.Fatalf("input = %v, want the big item replaced", input)
	}
	if small, _ := input[1].(map[string]any); small["content"] != "small" {
		t.Fatalf("the small item should be intact, got %v", input[1])
	}
	if m["system_instructions"] != PayloadCapMarker || !strings.Contains(PayloadCapMarker, "trace_span_data_kb") {
		t.Fatalf("instructions = %v, want the marker naming the setting", m["system_instructions"])
	}
	if out, _ := m["output"].([]any); len(out) != 1 {
		t.Fatalf("output = %v, want untouched", m["output"])
	}
}

// A fork copies the spans of the given runs with exactly the elements they
// reference — not the source session's whole store.
func TestTraceForkCopiesTheBlobsItReferences(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	w := ts.NewSpanWriter(id("s1"), 0)
	r1 := `{"input":[` + item("user", "a") + `]}`
	r2 := `{"input":[` + item("user", "a") + `,` + item("user", "b") + `]}`
	if err := w.Insert(ctx, &TraceEvent{RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: r1}); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(ctx, &TraceEvent{RunID: id("r2"), Kind: "span", SpanID: "g2", Name: "generation", Detail: "generation", Data: r2}); err != nil {
		t.Fatal(err)
	}

	if err := ts.ForkBySession(ctx, id("s1"), id("s2"), []string{id("r1")}); err != nil {
		t.Fatal(err)
	}
	if n := blobCount(t, ts, id("s2")); n != 1 {
		t.Fatalf("fork of r1 holds %d blobs, want 1", n)
	}
	if got, err := ts.GetBySpan(ctx, id("s2"), "g1"); err != nil || !sameJSON(got.Data, r1) {
		t.Fatalf("forked span = %+v (%v)", got, err)
	}
	if _, err := ts.GetBySpan(ctx, id("s2"), "g2"); err == nil {
		t.Fatal("r2's span should not be in the fork")
	}
	if err := ts.ForkBySession(ctx, id("s1"), id("s3"), []string{id("r1"), id("r2")}); err != nil {
		t.Fatal(err)
	}
	if n := blobCount(t, ts, id("s3")); n != 2 {
		t.Fatalf("fork of both runs holds %d blobs, want 2", n)
	}
	if n := blobCount(t, ts, id("s1")); n != 2 {
		t.Fatalf("source holds %d blobs, want 2", n)
	}
}

// Row retention takes a session's blobs with its last row and leaves a
// session that still has rows whole, whatever it lost.
func TestTraceRetentionDropsBlobsWithTheLastRow(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	old := time.Now().UTC().AddDate(0, 0, -40)
	data := `{"input":[` + item("user", "a") + `]}`
	s1old := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}
	s2old := &TraceEvent{SessionID: id("s2"), RunID: id("r2"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}
	s2new := &TraceEvent{SessionID: id("s2"), RunID: id("r3"), Kind: "span", SpanID: "g2", Name: "generation", Detail: "generation", Data: data}
	for _, ev := range []*TraceEvent{s1old, s2old, s2new} {
		if err := ts.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, ts, s1old, old)
	backdate(t, ts, s2old, old)

	n, err := ts.DeleteOlderThan(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil || n != 2 {
		t.Fatalf("DeleteOlderThan = %d (%v), want 2 rows", n, err)
	}
	if n := blobCount(t, ts, id("s1")); n != 0 {
		t.Fatalf("s1 blobs = %d, want 0 with its last row", n)
	}
	if n := blobCount(t, ts, id("s2")); n != 1 {
		t.Fatalf("s2 blobs = %d, want its 1 kept", n)
	}
	if got, err := ts.GetBySpan(ctx, id("s2"), "g2"); err != nil || !sameJSON(got.Data, data) {
		t.Fatalf("s2's remaining span = %+v (%v)", got, err)
	}
	if n, err := ts.DeleteOlderThan(ctx, time.Now().UTC().AddDate(0, 0, -30)); err != nil || n != 0 {
		t.Fatalf("second prune = %d (%v)", n, err)
	}
}

// Payload retention strips an idle session: its blobs go, its rows stay and
// read as plain metadata rows — unmarked, so the panel fetches nothing.
func TestTracePayloadPruneKeepsTheRows(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	stale := time.Now().UTC().AddDate(0, 0, -10)
	data := `{"model":"m","input":[` + item("user", "a") + `]}`
	idle := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}
	activeOld := &TraceEvent{SessionID: id("s2"), RunID: id("r2"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}
	activeNew := &TraceEvent{SessionID: id("s2"), RunID: id("r3"), Kind: "span", SpanID: "g2", Name: "generation", Detail: "generation", Data: data}
	for _, ev := range []*TraceEvent{idle, activeOld, activeNew} {
		if err := ts.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, ts, idle, stale)
	backdate(t, ts, activeOld, stale)

	cutoff := time.Now().UTC().AddDate(0, 0, -7)
	n, err := ts.PrunePayloadBefore(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("PrunePayloadBefore = %d (%v), want 1 session", n, err)
	}
	if n := blobCount(t, ts, id("s1")); n != 0 {
		t.Fatalf("idle session blobs = %d, want 0", n)
	}
	rows, err := ts.ListSummaryBySession(ctx, id("s1"), "", 0)
	if err != nil || len(rows) != 1 || rows[0].PayloadOmitted || rows[0].Data != `{"model":"m"}` {
		t.Fatalf("idle session summary = %+v (%v), want an unmarked metadata row", rows, err)
	}
	if got, err := ts.GetBySpan(ctx, id("s1"), "g1"); err != nil || got.Data != `{"model":"m"}` {
		t.Fatalf("idle session span = %+v (%v)", got, err)
	}
	if n := blobCount(t, ts, id("s2")); n != 1 {
		t.Fatalf("active session blobs = %d, want 1", n)
	}
	if got, err := ts.GetBySpan(ctx, id("s2"), "g1"); err != nil || !sameJSON(got.Data, data) {
		t.Fatalf("active session's old span = %+v (%v), want its payload", got, err)
	}
	// Nothing left to prune: a stripped session is not counted again.
	if n, err := ts.PrunePayloadBefore(ctx, cutoff); err != nil || n != 0 {
		t.Fatalf("second prune = %d (%v)", n, err)
	}
}

// An element whose blob is gone reads as a marker in its place; the rest of
// the span is whole. That is the reader's contract, whatever removed it.
func TestTraceMissingBlobReadsAsPruned(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	if err := ts.Insert(ctx, &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation",
		Data: `{"input":[` + item("user", "a") + `,` + item("user", "b") + `]}`}); err != nil {
		t.Fatal(err)
	}
	gone := sha256.Sum256([]byte(item("user", "b")))
	if _, err := ts.db.NewDelete().Model((*TraceBlob)(nil)).Where("session_id = ? AND hash = ?", id("s1"), gone[:]).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	m := spanPayload(t, ts, id("s1"), "g1")
	input, _ := m["input"].([]any)
	if len(input) != 2 || input[1] != payloadPrunedMarker {
		t.Fatalf("input = %v, want the second item read as pruned", input)
	}
	if first, _ := input[0].(map[string]any); first["content"] != "a" {
		t.Fatalf("the first item should be intact, got %v", input[0])
	}
}

// Deleting a session's traces takes its blobs; another session's stay.
func TestTraceDeleteBySessionDropsBlobs(t *testing.T) {
	ctx := context.Background()
	ts := NewTraceStore(newTestDB(t))
	id := ids(t)
	data := `{"input":[` + item("user", "a") + `]}`
	for _, sid := range []string{id("s1"), id("s2")} {
		if err := ts.Insert(ctx, &TraceEvent{SessionID: sid, RunID: id("r1"), Kind: "span", SpanID: "g1", Name: "generation", Detail: "generation", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.DeleteBySession(ctx, id("s1")); err != nil {
		t.Fatal(err)
	}
	if n := blobCount(t, ts, id("s1")); n != 0 {
		t.Fatalf("deleted session blobs = %d", n)
	}
	if rows, err := ts.ListBySession(ctx, id("s1"), "", 0); err != nil || len(rows) != 0 {
		t.Fatalf("deleted session rows = %+v (%v)", rows, err)
	}
	if n := blobCount(t, ts, id("s2")); n != 1 {
		t.Fatalf("other session blobs = %d, want 1", n)
	}
}
