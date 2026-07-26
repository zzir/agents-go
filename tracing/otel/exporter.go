// Package otel exports agents-go traces as OpenTelemetry spans.
//
// It is a separate module so the OTel SDK — a heavy dependency with its own
// release cadence — stays out of the core. The core tracing package remains
// vendor-neutral: a span there is a flat record with string ids, and this
// package rebuilds the tree.
//
//	import agentsotel "github.com/zzir/agents-go/tracing/otel"
//
//	exp, err := agentsotel.NewExporter(agentsotel.Options{TracerProvider: tp})
//	if err != nil { return err }
//	tracer := tracing.NewTracer(tracing.NewBatchProcessor(exp, tracing.BatchProcessorOptions{}))
//
// # How the tree is rebuilt
//
// Our spans are exported in batches *after* they finish, so a child usually
// arrives before its parent. OpenTelemetry normally builds a tree by nesting
// live spans through a context, which is not available after the fact.
//
// The reconstruction works by pinning: for each span, a custom IDGenerator is
// set to the exact trace and span ids we already assigned, the parent is
// injected as a remote SpanContext, and the span is started and immediately
// ended with the original timestamps. Verified end to end, including children
// exported before their already-finished parent.
//
// The pinning makes span creation stateful, so Export serializes it under a
// mutex. That is fine for a batch exporter and is why this must not be used as
// a synchronous per-span processor.
package otel

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zzir/agents-go/tracing"
)

// Options configures an Exporter.
type Options struct {
	// TracerProvider receives the reconstructed spans. Required.
	//
	// It must be an *sdktrace.TracerProvider built with this package's
	// IDGenerator (see NewTracerProvider) — the reconstruction works by
	// pinning ids on that generator before each span starts.
	TracerProvider *sdktrace.TracerProvider

	// IDGenerator is the pinned generator given to TracerProvider. Required,
	// and it must be the same one.
	//
	// NewTracerProvider builds a matching pair; pass them here when you need to
	// configure the provider yourself.
	IDGenerator *IDGenerator

	// ProviderName is reported as gen_ai.provider.name on generation spans.
	// Defaults to "openai", the only provider the SDK talks to.
	ProviderName string

	// ScopeName names the instrumentation scope. Defaults to the module path.
	ScopeName string
}

const defaultScopeName = "github.com/zzir/agents-go/tracing/otel"

// Exporter implements tracing.Exporter, translating our spans into OTel spans.
//
// It must be driven by a batch processor. Export serializes internally because
// the id pinning it relies on is process-global state on the IDGenerator.
type Exporter struct {
	tp     *sdktrace.TracerProvider
	idGen  *IDGenerator
	tracer oteltrace.Tracer

	provider string

	mu sync.Mutex
	// traces remembers each trace's workflow metadata until its spans arrive.
	// Batches deliver the Trace record first (OnTraceStart enqueues it), but a
	// span whose trace was never seen still exports — with the workflow
	// attributes missing rather than dropped.
	traces map[string]*tracing.Trace
}

// NewExporter builds an Exporter. It returns an error rather than panicking on
// a missing provider, because a misconfigured exporter that silently drops
// every span is the failure mode this is guarding against.
func NewExporter(opts Options) (*Exporter, error) {
	if opts.TracerProvider == nil {
		return nil, errors.New("otel: Options.TracerProvider is required")
	}
	if opts.IDGenerator == nil {
		return nil, errors.New("otel: Options.IDGenerator is required and must be the one given to TracerProvider")
	}
	opts.ProviderName = cmp.Or(opts.ProviderName, "openai")
	opts.ScopeName = cmp.Or(opts.ScopeName, defaultScopeName)
	return &Exporter{
		tp:       opts.TracerProvider,
		idGen:    opts.IDGenerator,
		tracer:   opts.TracerProvider.Tracer(opts.ScopeName),
		provider: opts.ProviderName,
		traces:   make(map[string]*tracing.Trace),
	}, nil
}

// NewTracerProvider builds a TracerProvider wired to a fresh pinned
// IDGenerator, plus the Exporter that drives it. This is the path to take
// unless the provider needs options of its own.
func NewTracerProvider(sdkOpts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, *Exporter, error) {
	gen := NewIDGenerator()
	tp := sdktrace.NewTracerProvider(append(sdkOpts, sdktrace.WithIDGenerator(gen))...)
	exp, err := NewExporter(Options{TracerProvider: tp, IDGenerator: gen})
	if err != nil {
		return nil, nil, err
	}
	return tp, exp, nil
}

// Export translates a batch of items into OTel spans.
func (e *Exporter) Export(items []tracing.Item) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Record trace metadata first, so a batch carrying a trace and its spans
	// together stamps the workflow attributes correctly regardless of order.
	for _, item := range items {
		if tr, ok := item.(*tracing.Trace); ok {
			e.traces[tr.TraceID] = tr
		}
	}
	for _, item := range items {
		if span, ok := item.(*tracing.Span); ok {
			e.emit(span)
		}
	}
}

func (e *Exporter) emit(s *tracing.Span) {
	traceID, err := parseTraceID(s.TraceID)
	if err != nil {
		return // an id we did not mint; nothing sensible to export
	}
	spanID, err := parseSpanID(s.SpanID)
	if err != nil {
		return
	}

	ctx := context.Background()
	if parent, perr := parseSpanID(s.ParentID); perr == nil {
		// A remote parent: the real parent span is not live here (it may not
		// even have been exported yet), but its id is enough for the collector
		// to reassemble the tree.
		ctx = oteltrace.ContextWithSpanContext(ctx, oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     parent,
			TraceFlags: oteltrace.FlagsSampled,
			Remote:     true,
		}))
	}

	// Pin the ids the SDK is about to generate. This is why Export holds a
	// mutex: the pin is read by the generator during Start, so two concurrent
	// translations would take each other's ids.
	e.idGen.pin(traceID, spanID)
	name, attrs := describe(s, e.provider)
	if tr, ok := e.traces[s.TraceID]; ok && s.ParentID == "" {
		attrs = append(attrs,
			attribute.String(attrWorkflowName, tr.WorkflowName))
		if tr.GroupID != "" {
			attrs = append(attrs, attribute.String(attrTraceGroupID, tr.GroupID))
		}
	}

	_, otelSpan := e.tracer.Start(ctx, name,
		oteltrace.WithTimestamp(s.StartedAt),
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
		oteltrace.WithAttributes(attrs...),
	)
	if s.Error != nil {
		otelSpan.SetStatus(codes.Error, s.Error.Message)
		otelSpan.SetAttributes(attribute.String(attrErrorType, errorType(s)))
	}
	end := s.EndedAt
	if end.IsZero() {
		// A span that never finished (a crashed run) still exports, closed at
		// its start, rather than being silently dropped or given "now" — which
		// would report a duration equal to how long the batch sat in the queue.
		end = s.StartedAt
	}
	otelSpan.End(oteltrace.WithTimestamp(end))
}

// describe maps one of our spans to an OTel span name and attribute set,
// following the GenAI semantic conventions where they apply.
func describe(s *tracing.Span, provider string) (string, []attribute.KeyValue) {
	attrs := make([]attribute.KeyValue, 0, 8)
	str := func(key, dataKey string) string {
		v, _ := s.Data[dataKey].(string)
		if v != "" {
			attrs = append(attrs, attribute.String(key, v))
		}
		return v
	}

	// The Data keys below are the ones the runner actually writes, verified
	// against agents/run.go and run_step.go — not assumed. Where a span carries
	// both a constructor value and a richer one added later, the later one
	// wins: a generation span is built with "name" (the agent's configured
	// model) and annotated with "model" (what was actually called), and those
	// differ whenever RunOptions.Model.Override is set.
	switch s.Type {
	case tracing.SpanTypeAgent:
		attrs = append(attrs, attribute.String(attrOperationName, OpInvokeAgent))
		name := str(attrAgentName, "name")
		return spanName(OpInvokeAgent, name), attrs

	case tracing.SpanTypeGeneration:
		attrs = append(attrs,
			attribute.String(attrOperationName, OpChat),
			attribute.String(attrProviderName, provider))
		model := firstString(s, "model", "name")
		if model != "" {
			attrs = append(attrs, attribute.String(attrRequestModel, model))
		}
		str(attrResponseID, "response_id")
		if v, ok := intData(s, "input_tokens"); ok {
			attrs = append(attrs, attribute.Int64(attrUsageInputTokens, v))
		}
		if v, ok := intData(s, "output_tokens"); ok {
			attrs = append(attrs, attribute.Int64(attrUsageOutputTokens, v))
		}
		return spanName(OpChat, model), attrs

	case tracing.SpanTypeFunction:
		attrs = append(attrs, attribute.String(attrOperationName, OpExecuteTool))
		name := str(attrToolName, "name")
		return spanName(OpExecuteTool, name), attrs

	case tracing.SpanTypeHandoff:
		// The span carries the handoff TOOL name (transfer_to_billing), which
		// is what the model called. There is no from/to pair on the span, so
		// inventing one here would be fiction.
		str(attrHandoffTool, "name")
		return "handoff", attrs

	case tracing.SpanTypeGuardrail:
		str(attrGuardrailStage, "stage")
		return "guardrail", attrs

	case tracing.SpanTypeCompaction:
		// Items, not tokens: the compaction span counts entries, and calling
		// them tokens would be a plausible-looking lie in a dashboard.
		if v, ok := intData(s, "before_items"); ok {
			attrs = append(attrs, attribute.Int64(attrCompactionBefore, v))
		}
		if v, ok := intData(s, "after_items"); ok {
			attrs = append(attrs, attribute.Int64(attrCompactionAfter, v))
		}
		return "compact", attrs

	case tracing.SpanTypeModelRetry:
		// Not gen_ai.*: the conventions have no notion of a retried call, and
		// naming it as though they did would imply a portability that is not
		// there.
		if v, ok := intData(s, "attempt"); ok {
			attrs = append(attrs, attribute.Int64(attrRetryAttempt, v))
		}
		if v, ok := intData(s, "max_attempts"); ok {
			attrs = append(attrs, attribute.Int64(attrRetryMaxAttempts, v))
		}
		return "model_retry", attrs

	case tracing.SpanTypeMCP:
		// An MCP tool call IS a tool execution, so it carries the GenAI
		// operation and tool name; the server is ours.
		str(attrMCPServer, "server")
		if name := firstString(s, "tool"); name != "" {
			attrs = append(attrs,
				attribute.String(attrOperationName, OpExecuteTool),
				attribute.String(attrToolName, name))
			return spanName(OpExecuteTool, name), attrs
		}
		return s.Name, attrs

	case tracing.SpanTypeSandbox:
		if v, ok := intData(s, "exit_code"); ok {
			attrs = append(attrs, attribute.Int64(attrSandboxExitCode, v))
		}
		return s.Name, attrs
	}

	// An untyped span (tracing.StartSpan) carries whatever the caller put in
	// Data; pass it through under an agents. prefix rather than dropping it.
	for k, v := range s.Data {
		if sv, ok := v.(string); ok {
			attrs = append(attrs, attribute.String("agents."+k, sv))
		}
	}
	name := s.Name
	name = cmp.Or(name, "span")
	return name, attrs
}

// spanName follows the GenAI convention of "<operation> <target>", falling back
// to the bare operation when the target is unknown.
func spanName(op, target string) string {
	if target == "" {
		return op
	}
	return op + " " + target
}

// errorType reports a stable, low-cardinality error classification. The SDK's
// ErrorCode is exactly that, when the span recorded one.
func errorType(s *tracing.Span) string {
	if code, ok := s.Error.Data["code"].(string); ok && code != "" {
		return code
	}
	return "_OTHER"
}

// firstString returns the first of keys present in Data as a non-empty string.
func firstString(s *tracing.Span, keys ...string) string {
	for _, k := range keys {
		if v, ok := s.Data[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func intData(s *tracing.Span, key string) (int64, bool) {
	switch v := s.Data[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		// JSON round-trips numbers as float64.
		return int64(v), true
	}
	return 0, false
}

// parseTraceID converts our "trace_<32 hex>" id into an OTel TraceID.
func parseTraceID(id string) (oteltrace.TraceID, error) {
	raw, err := unprefix(id, "trace_", 16)
	if err != nil {
		return oteltrace.TraceID{}, err
	}
	var out oteltrace.TraceID
	copy(out[:], raw)
	return out, nil
}

// parseSpanID converts our "span_<16 hex>" id into an OTel SpanID. The widths
// match by construction (see tracing.NewSpanID), so no truncation happens here.
func parseSpanID(id string) (oteltrace.SpanID, error) {
	raw, err := unprefix(id, "span_", 8)
	if err != nil {
		return oteltrace.SpanID{}, err
	}
	var out oteltrace.SpanID
	copy(out[:], raw)
	return out, nil
}

func unprefix(id, prefix string, wantBytes int) ([]byte, error) {
	if !strings.HasPrefix(id, prefix) {
		return nil, fmt.Errorf("otel: %q is not a %s id", id, strings.TrimSuffix(prefix, "_"))
	}
	raw, err := hex.DecodeString(id[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("otel: %q is not hex: %w", id, err)
	}
	if len(raw) != wantBytes {
		return nil, fmt.Errorf("otel: %q is %d bytes, want %d", id, len(raw), wantBytes)
	}
	return raw, nil
}
