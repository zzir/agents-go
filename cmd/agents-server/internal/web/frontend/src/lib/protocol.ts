// Frontend mirror of the WebSocket protocol constants in
// internal/protocol/messages.go. Every ws.on(...)/ws.send(...) and every
// RunError.code branch must reference these — never a string literal — so a
// typo is a compile-time unknown property instead of an event that silently
// never fires. Keep in sync with the Go constants when adding an event.

export const EV = {
  // client → server
  auth: 'auth',
  runCreate: 'run.create',
  runCancel: 'run.cancel',
  runSubscribe: 'run.subscribe',
  toolApprove: 'tool.approve',
  toolReject: 'tool.reject',

  // server → client
  authOk: 'auth.ok',
  runStarted: 'run.started',
  runAgentStart: 'run.agent_start',
  runStep: 'run.step',
  runReasoning: 'run.reasoning',
  runMessage: 'run.message',
  runReasoningItem: 'run.reasoning_item',
  runToolCall: 'run.tool_call',
  runToolResult: 'run.tool_result',
  runHandoff: 'run.handoff',
  runOutput: 'run.output',
  runError: 'run.error',
  runInterrupted: 'run.interrupted',
  runCancelled: 'run.cancelled',
  runCompaction: 'run.compaction',
  sessionTitleUpdated: 'session.title_updated',
  traceSpan: 'trace.span',
} as const;

// RunError.code values the client branches on for recovery behavior.
export const ERR = {
  sessionBusy: 'session_busy',
  sessionNotFound: 'session_not_found',
  runNotFound: 'run_not_found',
  approvalFailed: 'approval_failed',
  guardrailTripwire: 'guardrail_tripwire',
} as const;
