// ItemDisplay mirrors the SDK's agents.ItemDisplay: what the RUNNER decided an
// entry looks like, recorded when it happened. The frontend never parses
// wire-format item JSON, and the server no longer re-derives this at read time —
// it only ever produced a worse version of what the SDK already knew.
interface ItemDisplay {
  kind: string;
  renderer?: string;
  // title/summary are the producer's card heading and one-liner (a task's
  // label, "3 files changed"); empty falls back to tool_name / the existing
  // rendering, per the display contract.
  title?: string;
  summary?: string;
  text?: string;
  call_id?: string;
  tool_name?: string;
  arguments?: string;
  output?: string;
  is_error?: boolean;
  // extra is whatever a tool attached via Details, plus the fields the server
  // amends onto a card afterwards: a guardrail block's name/stage, a spawned
  // task's id and terminal status (the durable truth the task card is rebuilt
  // from on reload, since the hub run is GC'd).
  extra?: DisplayExtra;
}

// DisplayExtra is the shape of a display's `extra` bag wherever it travels:
// the stored entry's display, the live run.tool_result event, and the ToolCall
// both fold it into. One name, so the three cannot drift.
interface DisplayExtra {
  guardrail?: string;
  stage?: string;
  task_id?: string;
  task_status?: string;
  // Which run of the task this describes: 1 for the original, more after a
  // retry. A card showing a finished task uses it to tell a NEW attempt from a
  // replay of the one it already has.
  task_attempt?: number;
  // Which attempt wrote the folded summary. The fold keeps the last NON-EMPTY
  // summary (a later update cannot blank one), so after a retry the previous
  // attempt's failure text survives beside the new attempt's status — this is
  // the provenance that lets assemble drop it.
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
} as const;

// EntryView is one row of GET /sessions/:id/messages — a stored session entry
// plus the row id the cursor pages on. Update entries are already folded into
// their targets server-side, so nothing here needs to apply them.
interface EntryView {
  id?: number;
  entry_id?: string;
  parent_id?: string;
  kind: string;
  run_id?: string;
  role: string;
  content?: string;
  display?: ItemDisplay;
  compacted?: boolean;
  // on_path is false for an abandoned attempt: still recorded, still
  // switchable-to, but not part of the conversation as it currently stands.
  on_path?: boolean;
  compaction?: CompactionInfo;
}

// Branches describes one fork point: the sibling attempts that hang off a
// shared parent, and which of them the session is currently on.
interface Branches {
  // parentId is the entry they all continue from.
  parentId: string;
  // tips are one entry id per attempt, in the order they were made. Switching
  // to one means branching to its tip.
  tips: string[];
  // active indexes tips — which attempt is on the current path.
  active: number;
}

// CompactionInfo is present on a checkpoint entry: what the pass folded away.
interface CompactionInfo {
  excluded_ids?: string[];
  tokens_before?: number;
  tokens_after?: number;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return '<1s';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + 's';
  const m = Math.floor(s / 60);
  const rs = Math.round(s % 60);
  if (m < 60) return m + 'm ' + rs + 's';
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return h + 'h ' + rm + 'm';
}

// The ONE ToolCall / TurnPart definition — the streaming path (streamReducer),
// the replay path (buildTimeline) and every renderer (ChatView, ToolCallCard)
// import these. A second local copy is how the two paths drift apart.
interface ToolCall {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  output: string | null;
  status: string | null;
  needs_approval?: boolean;
  // title/summary are the tool's display overrides from its result (a card
  // heading over the tool name, a one-line account of what happened). Set by
  // both paths — the live run.tool_result event and the stored output entry's
  // display — so the card reads the same before and after a reload.
  title?: string;
  summary?: string;
  // is_error marks a result that reports a failure.
  is_error?: boolean;
  // extra is the result's Details bag (a task result's task_id, an
  // exec_command's command), folded from the same two paths as title/summary.
  extra?: DisplayExtra;
  // Terminal task outcome for a spawn_task call, from the display projection.
  task?: { id?: string; label?: string; status?: string; summary?: string; attempt?: number };
  // progress is live output the tool pushed while still running — a command's
  // stdout as it appears, a sub-agent thinking out loud. It is NOT the result:
  // `output` is, and it replaces this when it lands. Live-only, so a reload
  // shows the result rather than a replay of how it got there.
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

// A live-only marker for an agent switch (run.handoff), rendered inside the
// turn's process timeline. Never persisted — on reload the transfer_to_* tool
// call card conveys the same information.
interface HandoffPart {
  type: 'handoff';
  content: string;
}

type TurnPart = ToolsPart | TextPart | ErrorPart | CancelledPart | ThinkingPart | HandoffPart;

interface UserEntry {
  role: 'user';
  content: string;
  // Absent on entries not yet persisted: the sender's optimistic bubble and
  // the bubble a watching browser builds from run.started's input.
  messageId?: number;
  // entryId is the durable entry id, which is what a branch switch aims at —
  // messageId is a row id, and branching is expressed in entry ids.
  entryId?: string;
  runId?: string;
}

interface SystemEntry {
  role: 'system';
  content: string;
  messageId: number | undefined;
}

interface TurnEntry {
  role: 'turn';
  parts: TurnPart[];
  // Persisted turns carry the anchoring row id; a live turn assembled from
  // stream events has none until the post-run reload swaps it in.
  messageId?: number;
  runId?: string;
  // Set when this turn is one of several attempts at the same point, so the
  // renderer can offer "2 / 3 ‹ ›" instead of silently showing one of them.
  branches?: Branches;
}

interface CompactionEntry {
  role: 'compaction';
  content: string;
  messageId: number | undefined;
  // folded are the entries this checkpoint stands in for, rendered inside it
  // rather than loose in the history. They are still the real entries — a
  // compaction soft-deletes from the MODEL's context, not from what happened —
  // so collapsing them is a display choice the reader can undo.
  folded?: TimelineEntry[];
  tokensBefore?: number;
  tokensAfter?: number;
}

type TimelineEntry = UserEntry | SystemEntry | TurnEntry | CompactionEntry;

interface HookEvent {
  agent_name?: string;
  tool_name?: string;
  from?: string;
  to?: string;
  detail?: string;
}

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

export type { EntryView, ItemDisplay, DisplayExtra, CompactionInfo, CompactionEntry, Branches, ToolCall, ToolsPart, TextPart, ErrorPart, CancelledPart, ThinkingPart, HandoffPart, TurnPart, TurnEntry, UserEntry, TimelineEntry, HookEvent, ToolCallPatch };
export { DISPLAY };

// buildTimeline folds a session's entries into the rendered timeline.
//
// It dispatches on the entry's KIND and its recorded display kind, not on a
// role string the server invented per row. That is the whole point of the entry
// model: the runner knew this was a tool call when it made one, and the reader
// should not be re-deducing it from a projection.
//
// Entries a compaction checkpoint folded away are moved INSIDE that checkpoint
// rather than left loose in the history: a reader scrolling back should see
// "12k tokens became 3k here" as one marker, not the folded turns rendered as
// though the model still reads them. They stay real and expandable — compaction
// soft-deletes from the model's context, never from what happened.
export function buildTimeline(entries: EntryView[] | null | undefined): TimelineEntry[] {
  if (!entries) return [];

  // Off-path entries are abandoned attempts. They are dropped from the rendered
  // conversation — showing both answers to the same question inline would be
  // showing a conversation that never happened — and surfaced instead as the
  // "2 / 3" switcher on the attempt that IS current. The filter is NOT gated
  // on a fork existing: right after a regenerate's branch switch the abandoned
  // attempt is still the user message's ONLY child (the new attempt has not
  // persisted anything yet, and the switch's leaf is not a child), so a fork
  // gate would leave the old answer on screen for the whole regeneration.
  const forks = findForks(entries);
  entries = entries.filter(e => e.on_path !== false);

  // Which checkpoint folded each entry. A checkpoint is appended AFTER what it
  // folds, so this needs its own pass. A later checkpoint wins when two name
  // the same entry, which is what puts a re-compacted range under the newest
  // marker (and the older checkpoint itself under it too).
  const foldedBy = new Map<string, string>();
  for (const e of entries) {
    if (e.kind !== 'compaction' || !e.entry_id) continue;
    for (const id of e.compaction?.excluded_ids || []) foldedBy.set(id, e.entry_id);
  }
  if (foldedBy.size === 0) return assemble(entries, null, forks);

  const buckets = new Map<string, EntryView[]>();
  const main: EntryView[] = [];
  for (const e of entries) {
    const owner = e.entry_id ? foldedBy.get(e.entry_id) : undefined;
    if (!owner) { main.push(e); continue; }
    const bucket = buckets.get(owner);
    if (bucket) bucket.push(e); else buckets.set(owner, [e]);
  }
  return assemble(main, buckets, forks);
}

// findForks locates every point where the conversation was answered more than
// once, keyed by the id of the child that continues each attempt.
//
// Leaf and update entries are excluded from the parent index on purpose: they
// are metadata appended at whatever the tip happened to be, so counting them as
// children would invent a fork at every branch switch.
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

  // tipOf walks an attempt to its last entry — where a switch has to branch to,
  // since branching to the middle of an attempt would truncate it.
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
    // Keyed by the ACTIVE child: that is the one still in the timeline, and the
    // switcher rides on the turn it starts.
    out.set(kids[branches.active], branches);
  }
  return out;
}

function assemble(
  entries: EntryView[],
  buckets: Map<string, EntryView[]> | null,
  forks: Map<string, Branches>,
): TimelineEntry[] {
  const timeline: TimelineEntry[] = [];
  const pendingTC: Record<string, ToolCall> = {};
  let turn: TurnEntry | null = null;
  const ensureTurn = (): void => {
    if (!turn) { turn = { role: 'turn', parts: [], messageId: 0 }; timeline.push(turn); }
  };
  const finishTurn = (): void => { turn = null; };
  // anchor pins the turn to the row it last absorbed, so a fork or a scroll
  // restore has a durable id to aim at.
  const anchor = (e: EntryView): void => {
    if (e.id) turn!.messageId = e.id;
    if (e.run_id) turn!.runId = e.run_id;
    // A turn that STARTS an attempt carries its switcher. Only the first entry
    // of the turn can, which is what `!turn.branches` guards.
    const b = e.entry_id ? forks.get(e.entry_id) : undefined;
    if (b && !turn!.branches) turn!.branches = b;
  };

  for (const e of entries) {
    const d = e.display;
    // A compaction checkpoint: what the pass folded away, standing in for it.
    if (e.kind === 'compaction') {
      finishTurn();
      const inner = (buckets && e.entry_id) ? buckets.get(e.entry_id) : undefined;
      timeline.push({
        role: 'compaction',
        content: e.content || '',
        messageId: e.id,
        folded: inner ? assemble(inner, buckets, forks) : undefined,
        tokensBefore: e.compaction?.tokens_before,
        tokensAfter: e.compaction?.tokens_after,
      });
      continue;
    }
    // An entry marked compacted but named by no checkpoint still renders in
    // place: it is history, and the model no longer reading it is not a reason
    // to hide it. Dropping these outright made whole runs vanish.
    if (e.role === 'user') {
      finishTurn();
      if (e.content) timeline.push({ role: 'user', content: e.content, messageId: e.id, entryId: e.entry_id, runId: e.run_id });
      continue;
    }
    switch (d?.kind) {
      case DISPLAY.toolCall: {
        if (!d.call_id) break;
        ensureTurn();
        anchor(e);
        const x = d.extra;
        const tc: ToolCall = { tool_call_id: d.call_id, tool_name: d.tool_name || '', arguments: d.arguments || '', output: null, status: null };
        if (x?.task_id || x?.task_status) {
          // A summary from an earlier attempt than the card's is a leftover a
          // retry voided, not the current result — the fold cannot blank it
          // (only non-empty fields merge), so compare its provenance instead.
          // Rows written before attempts existed carry neither key and keep
          // their summary.
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
          // The output entry's display carries the tool's word on how to
          // present the result. Applied conditionally, mirroring the live
          // path (applyToolResult) — the isomorphism test compares the two.
          if (d.title) tc.title = d.title;
          if (d.summary) tc.summary = d.summary;
          if (d.renderer) tc.renderer = d.renderer;
          if (d.is_error) tc.is_error = true;
          // Non-empty only: the wire omits an empty bag (omitempty), so an
          // empty stored one must fold to nothing too or the paths diverge.
          if (d.extra && Object.keys(d.extra).length) tc.extra = d.extra;
        }
        continue;
      }
      case DISPLAY.error: {
        if (!e.content) continue;
        ensureTurn();
        anchor(e);
        turn!.parts.push({ type: 'error', content: e.content, guardrail: d.extra?.guardrail, stage: d.extra?.stage });
        continue;
      }
      case DISPLAY.cancelled: {
        // A run stopped by the user (or a deadline). Content is optional — the
        // card renders a fixed label — so this branch does not gate on it.
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
      case DISPLAY.message: {
        // Assistant prose. Matching the display kind (not the role) keeps it
        // rendering as markdown text even when provenance maps the role
        // elsewhere: a failed run's partial text (an annotation) and an
        // error-handler fallback answer both used to arrive as role "system"
        // and got squeezed into a single-line system chip.
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

// findToolCall returns the tool call with the given id (searching newest-first),
// or null. Used to read a call's current state before patching it.
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
