package otel

// OpenTelemetry GenAI semantic-convention attribute keys.
//
// Pinned to **v1.38.0** of the semantic conventions. The GenAI conventions are
// still marked experimental upstream and have renamed keys between releases
// (gen_ai.system became gen_ai.provider.name, for one), so the version is
// recorded here rather than left to whatever the OTel module happens to ship.
// Changing it is a deliberate edit, not a dependency bump side effect.
//
// Keys with no GenAI equivalent — handoffs, guardrails, compaction — use an
// `agents.` prefix. They are ours, and naming them `gen_ai.*` would imply a
// convention that does not cover them.
const (
	// SemConvVersion is the semantic-convention release these keys track.
	SemConvVersion = "1.38.0"

	// Operation names (gen_ai.operation.name).
	OpInvokeAgent = "invoke_agent"
	OpChat        = "chat"
	OpExecuteTool = "execute_tool"

	attrOperationName = "gen_ai.operation.name"
	attrProviderName  = "gen_ai.provider.name"

	attrAgentName = "gen_ai.agent.name"

	attrRequestModel      = "gen_ai.request.model"
	attrResponseID        = "gen_ai.response.id"
	attrUsageInputTokens  = "gen_ai.usage.input_tokens"
	attrUsageOutputTokens = "gen_ai.usage.output_tokens"

	attrToolName = "gen_ai.tool.name"

	// error.type is a general OTel convention, not GenAI-specific.
	attrErrorType = "error.type"

	// Ours: no GenAI convention covers these.
	attrHandoffTool    = "agents.handoff.tool"
	attrGuardrailStage = "agents.guardrail.stage"
	// Items, not tokens — the compaction span counts entries.
	attrCompactionBefore = "agents.compaction.before_items"
	attrCompactionAfter  = "agents.compaction.after_items"

	attrRetryAttempt     = "agents.model.retry.attempt"
	attrRetryMaxAttempts = "agents.model.retry.max_attempts"
	attrMCPServer        = "agents.mcp.server"
	attrSandboxExitCode  = "agents.sandbox.exit_code"

	// Trace-level, stamped on the root span of each trace.
	attrWorkflowName = "agents.workflow.name"
	attrTraceGroupID = "agents.trace.group_id"
	// Prefix for tracing.WithMetadata entries: one attribute per key.
	attrMetadataPrefix = "agents.metadata."
)
