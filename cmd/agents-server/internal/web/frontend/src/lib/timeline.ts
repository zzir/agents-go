import type { AttachmentMeta } from '@/lib/attachments';
// ItemDisplay mirrors the SDK's agents.ItemDisplay: what the runner decided an
// entry looks like, recorded when it happened. The frontend never parses
// wire-format item JSON.
interface ItemDisplay {
  kind: string;
  renderer?: string;
  // title/summary are the producer's card heading and one-liner; empty falls
  // back to tool_name / the default rendering.
  title?: string;
  summary?: string;
  text?: string;
  call_id?: string;
  tool_name?: string;
  arguments?: string;
  output?: string;
  is_error?: boolean;
  // extra is whatever a tool attached via Details, plus what the server amends
  // onto a card afterwards (a guardrail block's name/stage, a spawned task's id
  // and terminal status).
  extra?: DisplayExtra;
}

// DisplayExtra is the shape of a display's `extra` bag wherever it travels
// (stored entry, live run.tool_result, the ToolCall both fold into) — one name,
// so the three cannot drift.
interface DisplayExtra {
  // On an error annotation: the run.error code.
  code?: string;
  guardrail?: string;
  stage?: string;
  // On a tool_call an abandoned pause never ran: the run.cancelled reason.
  not_run?: string;
  task_id?: string;
  task_status?: string;
  // Which run of the task this describes (1 for the original); tells a new
  // attempt from a replay.
  task_attempt?: number;
  // Which attempt wrote the folded summary. The fold keeps the last non-empty
  // summary, so after a retry this provenance is what lets assemble drop the
  // previous attempt's text.
  task_summary_attempt?: number;
  [k: string]: unknown;
}

// Display kinds. Mirrors agents/run_items.go — keep in sync.
const DISPLAY = {
  message: 'message',
  toolCall: 'tool_call',
  toolOutput: 'tool_output',
  reasoning: 'reasoning',
  handoff: 'handoff',
  error: 'error',
  cancelled: 'cancelled',
  // A person's or a trigger's start of a workflow (store.DisplayWorkflowStarted).
  workflowStarted: 'workflow_started',
  // A trigger's agent turn: the note before the message it sends
  // (store.DisplayTriggerFired).
  triggerFired: 'trigger_fired',
} as const;

// EntryView is one row of GET /sessions/:id/messages: a stored entry plus its row id.
// Update entries are already folded into their targets server-side.
interface EntryView {
  id?: string;
  entry_id?: string;
  parent_id?: string;
  kind: string;
  run_id?: string;
  role: string;
  content?: string;
  display?: ItemDisplay;
  compacted?: boolean;
  // on_path is false for an abandoned attempt: still recorded and
  // switchable-to, not on the current path.
  on_path?: boolean;
  compaction?: CompactionInfo;
  // Image attachments the entry's message carries, URL-resolved server-side.
  attachments?: AttachmentMeta[];
  created_at?: string;
}

// Branches describes one fork point: the sibling attempts that hang off a
// shared parent, and which of them the session is currently on.
interface Branches {
  // parentId is the entry they all continue from.
  parentId: string;
  // tips are one entry id per attempt, in the order made; switching means
  // branching to its tip.
  tips: string[];
  // active indexes tips — which attempt is on the current path.
  active: number;
}

// CompactionInfo is present on a checkpoint entry: what the pass folded away.
interface CompactionInfo {
  excluded_ids?: string[];
  tokens_before?: number;
  tokens_after?: number;
  reset?: boolean;
}

// The ONE ToolCall / TurnPart definition, imported by the streaming path
// (streamReducer), the replay path (buildTimeline) and every renderer —
// invariant 16.
interface ToolCall {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  output: string | null;
  // approved / rejected (live), completed, or not_run — a call an abandoned
  // pause never ran, with not_run saying why (a run.cancelled reason).
  status: string | null;
  not_run?: string;
  needs_approval?: boolean;
  // title/summary are the tool's display overrides from its result (card
  // heading, one-line account), set by both paths so the card reads the same
  // before and after a reload.
  title?: string;
  summary?: string;
  // is_error marks a result that reports a failure.
  is_error?: boolean;
  // extra is the result's Details bag (a task_id, a command), folded from the
  // same two paths.
  extra?: DisplayExtra;
  // Terminal task outcome for a spawn_task call, from the display projection.
  task?: { id?: string; label?: string; status?: string; summary?: string; attempt?: number };
  // progress is live output the tool pushed while running — NOT the result, which
  // `output` is and which replaces it. Live-only: a reload shows the result alone.
  progress?: string;
  // renderer is the tool's display hint for progress (e.g. "terminal").
  renderer?: string;
}

interface ToolsPart {
  type: 'tools';
  toolCalls: ToolCall[];
}

interface TextPart {
  type: 'text';
  content: string;
}

interface ErrorPart {
  type: 'error';
  content: string;
  // The run.error code, the same live and after a reload (invariant 16).
  code?: string;
  guardrail?: string;
  stage?: string;
}

interface CancelledPart {
  type: 'cancelled';
  content?: string;
}

interface ThinkingPart {
  type: 'thinking';
  content: string;
}

// A live-only marker for an agent switch (run.handoff) in the turn's process
// timeline; on reload the transfer_to_* tool-call card conveys the same.
interface HandoffPart {
  type: 'handoff';
  content: string;
  // The halves of content ("from → to"), with the config ids behind the names
  // when known, for the avatars.
  from?: string;
  to?: string;
  fromId?: string;
  toId?: string;
}

type TurnPart = ToolsPart | TextPart | ErrorPart | CancelledPart | ThinkingPart | HandoffPart;

interface UserEntry {
  role: 'user';
  content: string;
  // Absent on entries not yet persisted (an optimistic bubble, one built from
  // run.started).
  messageId?: string;
  // entryId is the durable entry id, which a branch switch aims at (messageId
  // is a row id).
  entryId?: string;
  runId?: string;
  // The message's image attachments, for the thumbnail grid.
  attachments?: AttachmentMeta[];
  // Stamped on this browser's own not-yet-sent bubble: what a rollback finds,
  // and what tells identical sends apart.
  clientMsgId?: string;
  // Set on a live bubble the run read from its queue (run.injected's index); it
  // shares the prompt's run id.
  injected?: number;
  // When the message was recorded (ms), for placing a queued input on a run's trace.
  createdAt?: number;
}

// WorkflowStartedNote is the data of a started note: a workflow's start (taskId
// pairs it with the result's wake-up run), or a trigger's agent turn before the
// message it sends.
interface WorkflowStartedNote {
  taskId: string;
  workflowId: string;
  workflowName: string;
  // A trigger's agent turn, instead of a workflow.
  agentName?: string;
  agentConfigId?: string;
  runId?: string;
  brief: string;
  origin: { kind: string; trigger_id?: string; trigger_kind?: string; schedule?: string };
}

interface SystemEntry {
  role: 'system';
  content: string;
  messageId: string | undefined;
  // The durable entry id, for anchors.
  entryId?: string;
  // Present on a workflow-started note; the chip renders it instead of content.
  note?: WorkflowStartedNote;
}

interface TurnEntry {
  role: 'turn';
  parts: TurnPart[];
  // The anchoring row id; a live turn has none until the post-run reload swaps it in.
  messageId?: string;
  runId?: string;
  // Set when this turn is one of several attempts at the same point ("2 / 3 ‹ ›").
  branches?: Branches;
}

interface CompactionEntry {
  role: 'compaction';
  content: string;
  messageId: string | undefined;
  // The durable entry id, the Context panel's jump target.
  entryId?: string;
  tokensBefore?: number;
  tokensAfter?: number;
  // A reset folded the conversation carrying the session memory, not a summary.
  reset?: boolean;
  // How many entries the checkpoint folded.
  foldedCount?: number;
}

type TimelineEntry = UserEntry | SystemEntry | TurnEntry | CompactionEntry;

interface ToolCallPatch {
  output?: string;
  status?: string | null;
  tool_name?: string;
  arguments?: string;
  needs_approval?: boolean;
  progress?: string;
  renderer?: string;
  title?: string;
  summary?: string;
  is_error?: boolean;
  extra?: DisplayExtra;
  task?: ToolCall['task'];
}

export type { EntryView, ItemDisplay, DisplayExtra, CompactionInfo, CompactionEntry, Branches, ToolCall, ToolsPart, TextPart, ErrorPart, CancelledPart, ThinkingPart, HandoffPart, TurnPart, TurnEntry, UserEntry, SystemEntry, WorkflowStartedNote, TimelineEntry, ToolCallPatch };

// originText says who started the execution, as the trace card and the chip
// both phrase it.
export function originText(origin: WorkflowStartedNote['origin']): string {
  if (origin.kind !== 'trigger') return 'you';
  const kind = origin.trigger_kind || 'trigger';
  return origin.schedule ? `${kind} ${origin.schedule}` : kind;
}

// buildTimeline folds a session's entries into the rendered timeline by entry kind and
// display kind; folded entries render in place, the checkpoint inline — invariant 24.
export function buildTimeline(entries: EntryView[] | null | undefined): TimelineEntry[] {
  if (!entries) return [];

  // Off-path entries are dropped and surfaced as the switcher on the current
  // attempt — invariant 19.
  const forks = findForks(entries);
  return assemble(entries.filter(e => e.on_path !== false), forks);
}

// findForks locates every point answered more than once, keyed by the active
// child's id. Leaf and update entries stay out of the parent index: appended at
// whatever the tip was, they would invent a fork at every switch.
function findForks(entries: EntryView[]): Map<string, Branches> {
  const children = new Map<string, string[]>();
  const byId = new Map<string, EntryView>();
  for (const e of entries) {
    if (!e.entry_id || e.kind === 'leaf' || e.kind === 'update') continue;
    byId.set(e.entry_id, e);
    const p = e.parent_id || '';
    const kids = children.get(p);
    if (kids) kids.push(e.entry_id); else children.set(p, [e.entry_id]);
  }

  // tipOf walks an attempt to its last entry, where a switch has to branch to.
  const tipOf = (id: string): string => {
    const seen = new Set<string>();
    for (;;) {
      if (seen.has(id)) return id; // a cycle nothing should produce
      seen.add(id);
      const kids = children.get(id);
      if (!kids || kids.length === 0) return id;
      id = kids[kids.length - 1];
    }
  };

  const out = new Map<string, Branches>();
  for (const [parentId, kids] of children) {
    if (kids.length < 2) continue;
    const active = kids.findIndex(k => byId.get(k)?.on_path !== false);
    const branches: Branches = { parentId, tips: kids.map(tipOf), active: active < 0 ? 0 : active };
    // Keyed by the ACTIVE child: the one still in the timeline, whose turn
    // carries the switcher.
    out.set(kids[branches.active], branches);
  }
  return out;
}

function assemble(
  entries: EntryView[],
  forks: Map<string, Branches>,
): TimelineEntry[] {
  const timeline: TimelineEntry[] = [];
  const pendingTC: Record<string, ToolCall> = {};
  let turn: TurnEntry | null = null;
  const ensureTurn = (): void => {
    if (!turn) { turn = { role: 'turn', parts: [], messageId: '' }; timeline.push(turn); }
  };
  const finishTurn = (): void => { turn = null; };
  // anchor pins the turn to the row it last absorbed, a durable id for a fork
  // or a scroll restore.
  const anchor = (e: EntryView): void => {
    if (e.id) turn!.messageId = e.id;
    if (e.run_id) turn!.runId = e.run_id;
    // Only the first entry of a turn carries its switcher (`!turn.branches`).
    const b = e.entry_id ? forks.get(e.entry_id) : undefined;
    if (b && !turn!.branches) turn!.branches = b;
  };

  for (const e of entries) {
    const d = e.display;
    // A compaction checkpoint: an inline marker; the history it folded renders
    // in place above it.
    if (e.kind === 'compaction') {
      finishTurn();
      timeline.push({
        role: 'compaction',
        content: e.content || '',
        messageId: e.id,
        reset: e.compaction?.reset,
        foldedCount: e.compaction?.excluded_ids?.length,
        entryId: e.entry_id,
        tokensBefore: e.compaction?.tokens_before,
        tokensAfter: e.compaction?.tokens_after,
      });
      continue;
    }
    if (e.role === 'user') {
      finishTurn();
      // An image-only message has no text — the attachments alone earn the bubble.
      if (e.content || e.attachments?.length) {
        timeline.push({
          role: 'user', content: e.content || '', messageId: e.id, entryId: e.entry_id, runId: e.run_id, attachments: e.attachments,
          createdAt: e.created_at ? Date.parse(e.created_at) || undefined : undefined,
        });
      }
      continue;
    }
    switch (d?.kind) {
      case DISPLAY.toolCall: {
        if (!d.call_id) break;
        ensureTurn();
        anchor(e);
        const x = d.extra;
        const tc: ToolCall = { tool_call_id: d.call_id, tool_name: d.tool_name || '', arguments: d.arguments || '', output: null, status: null };
        if (typeof x?.not_run === 'string') { tc.status = 'not_run'; tc.not_run = x.not_run; }
        if (x?.task_id || x?.task_status) {
          // A summary from an earlier attempt than the card's is a leftover a
          // retry voided; the fold cannot blank it, so compare provenance. Rows
          // from before attempts existed keep theirs.
          const stale = typeof x.task_attempt === 'number' && (x.task_summary_attempt ?? 0) < x.task_attempt;
          tc.task = { id: x.task_id, label: d.title, status: x.task_status, summary: stale ? undefined : d.summary, attempt: x.task_attempt };
        }
        pendingTC[d.call_id] = tc;
        const last = turn!.parts[turn!.parts.length - 1];
        if (last && last.type === 'tools') { (last as ToolsPart).toolCalls.push(tc); }
        else { turn!.parts.push({ type: 'tools', toolCalls: [tc] }); }
        continue;
      }
      case DISPLAY.toolOutput: {
        if (turn && e.id) (turn as TurnEntry).messageId = e.id;
        if (d.call_id && pendingTC[d.call_id]) {
          const tc = pendingTC[d.call_id];
          tc.output = d.output || e.content || '';
          tc.status = 'completed';
          // The output's display is applied conditionally, mirroring
          // applyToolResult — invariant 16.
          if (d.title) tc.title = d.title;
          if (d.summary) tc.summary = d.summary;
          if (d.renderer) tc.renderer = d.renderer;
          if (d.is_error) tc.is_error = true;
          // Non-empty only: the wire omits an empty bag, so a stored one must
          // fold to nothing too.
          if (d.extra && Object.keys(d.extra).length) tc.extra = d.extra;
        }
        continue;
      }
      case DISPLAY.error: {
        if (!e.content) continue;
        ensureTurn();
        anchor(e);
        turn!.parts.push({ type: 'error', content: e.content, code: d.extra?.code, guardrail: d.extra?.guardrail, stage: d.extra?.stage });
        continue;
      }
      case DISPLAY.cancelled: {
        // Content is optional (the card renders a fixed label), so this branch
        // does not gate on it.
        ensureTurn();
        anchor(e);
        turn!.parts.push({ type: 'cancelled', content: e.content || '' });
        continue;
      }
      case DISPLAY.reasoning: {
        if (!e.content) continue;
        ensureTurn();
        anchor(e);
        turn!.parts.push({ type: 'thinking', content: e.content });
        continue;
      }
      case DISPLAY.workflowStarted:
      case DISPLAY.triggerFired: {
        // A row of its own, like any system chip; the note's data is the
        // display's extra, the text its fallback.
        finishTurn();
        const x = d.extra || {};
        const origin = (x.origin && typeof x.origin === 'object') ? x.origin as WorkflowStartedNote['origin'] : { kind: 'person' };
        timeline.push({
          role: 'system', content: e.content || '', messageId: e.id, entryId: e.entry_id,
          note: {
            taskId: String(x.task_id || ''), workflowId: String(x.workflow_id || ''),
            workflowName: String(x.workflow_name || ''), brief: String(x.brief || ''), origin,
            ...(d.kind === DISPLAY.triggerFired ? { agentName: String(x.agent_name || ''), agentConfigId: String(x.agent_config_id || ''), runId: String(x.run_id || '') } : {}),
          },
        });
        continue;
      }
      case DISPLAY.message: {
        // Assistant prose, matched by display kind not role: a failed run's
        // partial text and an error-handler fallback arrive as role "system"
        // and still render as markdown.
        if (!e.content) continue;
        ensureTurn();
        anchor(e);
        turn!.parts.push({ type: 'text', content: e.content });
        continue;
      }
    }
    if (e.role === 'system' && e.content) {
      finishTurn();
      timeline.push({ role: 'system', content: e.content, messageId: e.id });
    } else if (e.content) {
      ensureTurn();
      anchor(e);
      turn!.parts.push({ type: 'text', content: e.content });
    }
  }
  finishTurn();
  return timeline;
}

// rowKeys gives every row a stable, unique React key: store id, else run id,
// else client id, else index, type-tagged (m/r/c/i) and role-prefixed; rows of
// one role sharing a run id (an injection split) take an ordinal.
export function rowKeys(messages: Array<{ role: string; messageId?: string | number; runId?: string; clientMsgId?: string }>): string[] {
  const seen = new Map<string, number>();
  return messages.map((m, i) => {
    const role = m.role === 'turn' || m.role === 'user' || m.role === 'compaction' ? m.role : 'msg';
    const base = m.messageId != null ? role + '-m' + m.messageId
      : m.runId ? role + '-r' + m.runId
        : m.clientMsgId ? role + '-c' + m.clientMsgId
          : role + '-i' + i;
    const n = seen.get(base) || 0;
    seen.set(base, n + 1);
    return n === 0 ? base : base + '~' + n;
  });
}

// findToolCall returns the tool call with the given id (searching
// newest-first), or null.
export function findToolCall(messages: TimelineEntry[], toolCallId: string): ToolCall | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== 'turn') continue;
    for (const part of (messages[i] as TurnEntry).parts) {
      if (part.type !== 'tools') continue;
      const tc = (part as ToolsPart).toolCalls.find(t => t.tool_call_id === toolCallId);
      if (tc) return tc;
    }
  }
  return null;
}

export function patchToolCall(messages: TimelineEntry[], toolCallId: string, patch: ToolCallPatch): TimelineEntry[] | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== 'turn') continue;
    const turnMsg = messages[i] as TurnEntry;
    const parts = [...turnMsg.parts];
    for (let j = parts.length - 1; j >= 0; j--) {
      if (parts[j].type !== 'tools') continue;
      const tcs = (parts[j] as ToolsPart).toolCalls;
      const idx = tcs.findIndex(tc => tc.tool_call_id === toolCallId);
      if (idx >= 0) {
        const newTcs = [...tcs];
        newTcs[idx] = { ...newTcs[idx], ...patch };
        parts[j] = { ...parts[j], toolCalls: newTcs } as ToolsPart;
        const newMsgs = [...messages];
        newMsgs[i] = { ...turnMsg, parts };
        return newMsgs;
      }
    }
  }
  return null;
}
