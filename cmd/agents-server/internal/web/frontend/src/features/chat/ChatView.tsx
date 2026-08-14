import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, memo, type MouseEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Button, Dialog, IconButton, Label, ActionMenu, ActionList, Select, Stack, TextInput } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useAsyncMarkdown, splitMermaidBlocks, sanitizeSVG } from '@/lib/markdown';
import { CHECK_ICON } from '@/lib/markdownShared';
import { formatDuration, type TurnPart, type ErrorPart, type CancelledPart, type TimelineEntry, type Branches, type EntryView } from '@/lib/timeline';
import { useScrollToBottom, useApi } from '@/lib/hooks';
import { loadSessionAgent, saveSessionAgent, loadSessionSandbox, saveSessionSandbox, loadSessionWorkdir, saveSessionWorkdir } from '@/lib/drafts';
import { bindingWorkDirIssue, composerSandboxView, groupProjects, projectLabel, type SessionBinding } from '@/lib/binding';
import { useRecentProjects } from '@/lib/useRecentProjects';
import { fc } from '@/lib/form';
import { parseTaskNotification, DIAGNOSTIC_LABELS, type TaskStatus, type RunDiagnostic } from '@/lib/protocol';
import { taskRetryable, type TaskState, type TaskViewState } from '@/lib/useAgentSocket';
import { mergeBackground, type BackgroundItem, type SessionApproval, type WorkflowRunRow } from '@/lib/background';
import { BackgroundListPanel, BackgroundDetailPanel } from '@/features/chat/BackgroundPanel';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { ChatToc } from '@/features/chat/ChatToc';
import { MessageInput } from '@/features/chat/MessageInput';
import { WorkflowStrip } from '@/features/chat/WorkflowStrip';
import { SlashMenu } from '@/features/chat/SlashMenu';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { TraceDrawer, type TraceEventData, type TraceReveal } from '@/features/chat/TracePanel';
import { ContextPanel } from '@/features/chat/ContextPanel';
import { ChatTopBar } from '@/features/chat/ChatTopBar';
import { ArrowDownIcon, ArrowSwitchIcon, ChevronRightIcon, ChevronLeftIcon, RepoForkedIcon, CopyIcon, CheckIcon, SyncIcon, CommentDiscussionIcon, PulseIcon, PlusIcon, DependabotIcon, CodeIcon, EyeIcon, AlertIcon, FileDirectoryIcon, LightBulbIcon, StopIcon, ShieldIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { toast } from '@/lib/toast';

/* ---------- types ---------- */

// `task` is the detail lens over ONE piece of background work — taskId is a
// task id or a workflow-execution id, whichever the list row was.
export type InspectorPanel = null | { kind: 'trace' } | { kind: 'tasks' } | { kind: 'task'; taskId: string } | { kind: 'context' };

interface ChatMessage {
  role: string;
  content?: string;
  // The run that produced this row — what groups a workflow's turns together.
  runId?: string;
  messageId?: string;
  // The durable entry id — what a branch switch and a regenerate aim at.
  entryId?: string;
  parts?: TurnPart[];
  // Present on a turn that is one of several attempts at the same point.
  branches?: Branches;
  // Present on a compaction checkpoint: the entries it folded away, and the
  // context size on either side of the pass.
  folded?: TimelineEntry[];
  tokensBefore?: number;
  tokensAfter?: number;
}

// Stable React key for a rendered message: prefer the durable store id, then
// the run id or the sender's optimistic client id, and only fall back to the
// array index for a transient entry that has none. Type-tagged prefixes
// (m/r/c/i) keep the id and index number-spaces from colliding; the role
// prefix keeps a user bubble and a turn that share a run id distinct. Plain
// index keys let collapse / copied state drift onto the wrong message whenever
// the list length changed (reload, fork, session switch).
function entryKey(
  m: { messageId?: string | number; runId?: string; clientMsgId?: string },
  i: number,
  role: string,
): string {
  if (m.messageId != null) return role + '-m' + m.messageId;
  if (m.runId) return role + '-r' + m.runId;
  if (m.clientMsgId) return role + '-c' + m.clientMsgId;
  return role + '-i' + i;
}


interface AgentConfig {
  id: string;
  name: string;
}

interface SandboxConfig {
  id: string;
  name: string;
  type?: string;
  // Whether this sandbox can host an interactive web terminal (server-computed:
  // ssh always, docker only when persistent, local never).
  terminal?: boolean;
  // The workdir a session binding would default to, and whether a custom
  // per-session workdir is honored (server-computed; docker constrains it to
  // the /workspace mount).
  default_work_dir?: string;
  work_dir_editable?: boolean;
}

interface MermaidSegment {
  type: 'md' | 'mermaid';
  text: string;
}

/* ---------- mermaid helpers ---------- */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let mermaidMod: any = null;
let mermaidTheme: string | null = null;
let mermaidIdSeq = 0;

function getColorModeEl(): Element {
  return document.querySelector('[data-color-mode]') || document.documentElement;
}

function getColorMode(): string {
  return getColorModeEl().getAttribute('data-color-mode') || 'light';
}

function primerThemeVars(): Record<string, string> {
  const el = document.querySelector('.app-layout') || document.documentElement;
  const s = getComputedStyle(el);
  const v = (n: string) => s.getPropertyValue(n).trim();
  return {
    fontFamily: v('--font-sans'),
    fontSize: '14px',

    primaryColor: v('--bgColor-muted'),
    primaryBorderColor: v('--borderColor-default'),
    primaryTextColor: v('--fgColor-default'),
    secondaryColor: v('--bgColor-neutral-muted'),
    secondaryBorderColor: v('--borderColor-default'),
    secondaryTextColor: v('--fgColor-default'),
    tertiaryColor: v('--bgColor-success-muted'),
    tertiaryBorderColor: v('--borderColor-default'),
    tertiaryTextColor: v('--fgColor-default'),

    lineColor: v('--fgColor-muted'),
    textColor: v('--fgColor-default'),
    mainBkg: v('--bgColor-muted'),
    nodeBorder: v('--borderColor-default'),
    nodeTextColor: v('--fgColor-default'),
    clusterBkg: v('--bgColor-default'),
    clusterBorder: v('--borderColor-muted'),
    titleColor: v('--fgColor-default'),
    edgeLabelBackground: v('--bgColor-default'),

    actorBkg: v('--bgColor-muted'),
    actorBorder: v('--borderColor-default'),
    actorTextColor: v('--fgColor-default'),
    signalColor: v('--fgColor-default'),
    signalTextColor: v('--fgColor-default'),
    labelBoxBkgColor: v('--bgColor-muted'),
    labelBoxBorderColor: v('--borderColor-default'),
    labelTextColor: v('--fgColor-default'),
    noteBkgColor: v('--bgColor-attention-muted'),
    noteBorderColor: v('--borderColor-default'),
    noteTextColor: v('--fgColor-default'),
    activationBorderColor: v('--borderColor-default'),
    activationBkgColor: v('--bgColor-muted'),
  };
}

async function ensureMermaid() {
  if (!mermaidMod) {
    mermaidMod = (await import('mermaid')).default;
  }
  const cur = getColorMode();
  if (mermaidTheme !== cur) {
    mermaidTheme = cur;
    mermaidMod.initialize({
      startOnLoad: false,
      theme: 'base',
      themeVariables: primerThemeVars(),
      flowchart: { useMaxWidth: false },
      sequence: { useMaxWidth: false },
      gantt: { useMaxWidth: false },
      journey: { useMaxWidth: false },
      class: { useMaxWidth: false },
      state: { useMaxWidth: false },
      er: { useMaxWidth: false },
      pie: { useMaxWidth: false },
      git: { useMaxWidth: false },
    });
  }
  return mermaidMod;
}

const MERMAID_CACHE_MAX = 100;
const mermaidCache = new Map<string, string>();
function mermaidCacheSet(key: string, value: string) {
  if (mermaidCache.size >= MERMAID_CACHE_MAX) {
    const first = mermaidCache.keys().next().value;
    if (first !== undefined) mermaidCache.delete(first);
  }
  mermaidCache.set(key, value);
}

/* ---------- SVG viewer overlay ---------- */

function SvgOverlay({ svg, onClose }: { svg: string; onClose: () => void }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', h);
    return () => document.removeEventListener('keydown', h);
  }, [onClose]);

  return createPortal(
    <div className="svg-overlay" onClick={onClose}>
      <div onClick={(e) => e.stopPropagation()} dangerouslySetInnerHTML={{ __html: svg }} />
    </div>,
    document.body,
  );
}

/* ---------- sub-components ---------- */

function MermaidBlock({ source }: { source: string }) {
  const [colorMode, setColorMode] = useState(getColorMode);
  const cacheKey = source + '\0' + colorMode;
  const cached = mermaidCache.get(cacheKey);
  const [svg, setSvg] = useState<string | null>(() => cached || null);
  const [failed, setFailed] = useState(false);
  const [viewerOpen, setViewerOpen] = useState(false);
  const [mode, setMode] = useState<'code' | 'svg'>('svg');
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    const target = getColorModeEl();
    const obs = new MutationObserver(() => setColorMode(getColorMode()));
    obs.observe(target, { attributes: true, attributeFilter: ['data-color-mode'] });
    return () => obs.disconnect();
  }, []);

  useEffect(() => {
    const c = mermaidCache.get(cacheKey);
    if (c) { setSvg(c); setFailed(false); return; }
    let cancelled = false;
    setFailed(false);
    (async () => {
      const id = `m${++mermaidIdSeq}`;
      try {
        const mermaid = await ensureMermaid();
        // `source` is the raw fenced ```mermaid``` body from the un-escaped
        // markdown — it must be fed to mermaid verbatim. Decoding entities here
        // corrupted diagrams that legitimately contain "&", "<" or ">".
        const { svg: rendered } = await mermaid.render(id, source);
        if (!cancelled) {
          const safe = sanitizeSVG(rendered);
          mermaidCacheSet(cacheKey, safe);
          setSvg(safe);
        }
      } catch {
        document.getElementById(id)?.remove();
        if (!cancelled) setFailed(true);
      }
    })();
    return () => { cancelled = true; };
  }, [source, cacheKey]);

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(source).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [source]);

  const showSvg = mode === 'svg' && svg && !failed;

  return (
    <div className="code-block-wrapper">
      <div className="code-block-actions">
        {svg && !failed && (
          <button
            className="btn-octicon btn-toggle-mermaid"
            aria-label={mode === 'svg' ? 'Show code' : 'Show diagram'}
            onClick={() => setMode(m => m === 'svg' ? 'code' : 'svg')}
          >
            {mode === 'svg' ? <CodeIcon size={16} /> : <EyeIcon size={16} />}
          </button>
        )}
        <button
          className={'btn-octicon btn-copy-react' + (copied ? ' copied' : '')}
          aria-label={copied ? 'Copied!' : 'Copy'}
          onClick={handleCopy}
        >
          {copied ? <CheckIcon size={16} /> : <CopyIcon size={16} />}
        </button>
      </div>
      {showSvg ? (
        <>
          <div className="mermaid-preview" onClick={() => setViewerOpen(true)} dangerouslySetInnerHTML={{ __html: svg }} />
          {viewerOpen && <SvgOverlay svg={svg} onClose={() => setViewerOpen(false)} />}
        </>
      ) : (
        <pre className="hljs-code-block"><code className="hljs">{source}</code></pre>
      )}
    </div>
  );
}

// STAGE_NOTES says what a trip at each stage actually stopped. A guardrail runs
// at four of them, and telling someone "the request was blocked before the
// model ran" when a tool result tripped it describes the wrong event.
const STAGE_NOTES: Record<string, string> = {
  input: 'The request was blocked before the model ran.',
  output: 'The response above was blocked before delivery.',
  tool_input: 'A tool call was blocked before the tool ran.',
  tool_output: "A tool's result was blocked before the model could read it.",
};

function ErrorCard({ message, guardrail, stage }: { message: string; guardrail?: string; stage?: string }) {
  // A guardrail block is not a system failure — render it as a distinct
  // "blocked" state.
  if (guardrail) {
    const label = `Blocked by guardrail “${guardrail}”`;
    const note = STAGE_NOTES[stage || ''] || 'The run was blocked by a guardrail.';
    return (
      <Disclosure icon={ShieldIcon} label={label} variant="attention" className="error-card">
        <pre className="error-card-body">{note + '\n\n' + message}</pre>
      </Disclosure>
    );
  }
  return (
    <Disclosure icon={AlertIcon} label="Error" variant="danger" className="error-card">
      <pre className="error-card-body">{message}</pre>
    </Disclosure>
  );
}

function CancelledCard() {
  return (
    <div className="cancelled-card">
      <StopIcon size={16} className="cancelled-card-icon" />
      <span>Run cancelled</span>
    </div>
  );
}

function MdSegment({ text }: { text: string }) {
  const html = useAsyncMarkdown(text);
  return <div dangerouslySetInnerHTML={{ __html: html }} />;
}

function TextContent({ content }: { content: string }) {
  const segments: MermaidSegment[] = useMemo(() => splitMermaidBlocks(content), [content]);
  return (
    <div className="turn-text markdown-body">
      {segments.map((seg, i) =>
        seg.type === 'mermaid'
          ? <MermaidBlock key={`m${i}`} source={seg.text} />
          : <MdSegment key={`t${i}`} text={seg.text} />
      )}
    </div>
  );
}

/* ---------- process timeline (thinking + tool calls) ---------- */

// Group a turn's parts into render segments: every text part is assistant
// prose said to the user — interim narration and final answer alike — and
// renders flat in chronological order; each unbroken run of thinking/tools
// parts between texts collapses into one process group. Notices (errors,
// cancellation) render separately at the end. Empty texts are dropped without
// splitting the group around them.
type TurnSegment =
  | { kind: 'text'; content: string }
  | { kind: 'process'; parts: TurnPart[] };

function buildSegments(parts: TurnPart[]): { segments: TurnSegment[]; notices: (ErrorPart | CancelledPart)[] } {
  const segments: TurnSegment[] = [];
  const notices: (ErrorPart | CancelledPart)[] = [];
  for (const p of parts) {
    if (p.type === 'error' || p.type === 'cancelled') { notices.push(p); continue; }
    if (p.type === 'text') {
      if (p.content) segments.push({ kind: 'text', content: p.content });
      continue;
    }
    const last = segments[segments.length - 1];
    if (last?.kind === 'process') last.parts.push(p);
    else segments.push({ kind: 'process', parts: [p] });
  }
  return { segments, notices };
}

function TimelineThinking({ content }: { content: string }) {
  return (
    <div className="pt-entry">
      <div className="pt-thinking">{content}</div>
    </div>
  );
}

function TimelineHandoff({ content }: { content: string }) {
  return (
    <div className="pt-entry">
      <div className="pt-handoff">
        <ArrowSwitchIcon size={14} />
        <span>{content}</span>
      </div>
    </div>
  );
}

interface ProcessTimelineProps {
  parts: TurnPart[];
  live: boolean;
  reasoning: string | null;
  // The turn's answer text has started streaming, so this group's thinking/tool
  // phase is done even while the run is still live.
  textStreaming?: boolean;
  compacting?: boolean;
  // Trouble this run survived, shown as a badge on the group header.
  diagnostics?: RunDiagnostic[];
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onInspectTask?: (taskId: string) => void;
  onRetryTask?: (taskId: string) => void;
  retryableByCallId?: Record<string, boolean>;
  liveTaskStatusByCallId?: Record<string, string>;
  liveTaskLabelByCallId?: Record<string, string>;
  taskLabelById?: Record<string, string>;
}

// Space-separated tool call ids across a turn's parts, for the group's
// data-anchor-ids attribute.
function toolCallIds(parts: TurnPart[]): string {
  return parts.flatMap(p => (p.type === 'tools' ? p.toolCalls.map(tc => tc.tool_call_id) : [])).join(' ');
}

// One collapsible group of thinking + tool-call parts. `live` marks the group
// still executing (the trailing one while its run is live): it stays open and
// shows a status label; settled groups collapse to "N steps".
function ProcessTimeline({ parts, live, reasoning, textStreaming, compacting, diagnostics, onApprove, onReject, onInspectTask, onRetryTask, retryableByCallId, liveTaskStatusByCallId, liveTaskLabelByCallId, taskLabelById }: ProcessTimelineProps) {
  // null = auto (open while live, closed once done); true/false = user override.
  const [expanded, setExpanded] = useState<boolean | null>(null);

  let stepCount = 0;
  let pendingCount = 0;
  let runningTool: string | null = null;
  let runningToolCount = 0;
  for (const p of parts) {
    if (p.type === 'tools') {
      stepCount += p.toolCalls.length;
      for (const tc of p.toolCalls) {
        if (tc.needs_approval && !tc.status) pendingCount++;
        else if (!tc.output && tc.status !== 'completed' && tc.status !== 'rejected') { runningToolCount++; if (!runningTool) runningTool = tc.tool_name; }
      }
    } else {
      stepCount++;
    }
  }
  if (live && reasoning) stepCount++;

  if (stepCount === 0) return null;

  // Once the turn's answer text starts streaming, this group's thinking/tool
  // phase is finished even though the run is still live: settle the label and
  // let it collapse like a done group, instead of pinning "Thinking…" over an
  // already-visible answer. The run.reasoning_item / run.message events that
  // freeze the live preview into parts only land after the whole model call, so
  // the live `reasoning` state otherwise lingers through the entire answer.
  const active = live && !textStreaming;

  const shouldShow = pendingCount > 0 || (expanded ?? active);

  // A pending approval is the group's status whether or not the run is still
  // "active": in the steady paused state running is false, so gate it above
  // `active` — otherwise it falls through to the settled step count and the
  // "Waiting for approval" wording only flashes in the run.tool_call→interrupted
  // window.
  const label = pendingCount > 0
    ? 'Waiting for approval'
    : active
      ? (compacting ? 'Compacting context…'
        : runningTool ? (runningToolCount > 1 ? 'Running ' + runningToolCount + ' tools…' : 'Running ' + runningTool + '…')
        : reasoning ? 'Thinking…' : 'Working…')
      : stepCount + ' step' + (stepCount > 1 ? 's' : '');

  return (
    // The call ids this group holds, so a jump aimed at a tool card inside a
    // COLLAPSED group can find the group (the cards themselves are not in the
    // DOM until it opens) and expand it.
    <div className="process-group" data-anchor-ids={toolCallIds(parts) || undefined}>
      <div
        className={'process-group-toggle' + (shouldShow ? ' expanded' : '')}
        onClick={() => setExpanded(!shouldShow)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>{label}</span>
        {pendingCount > 0 && <Label variant="accent" className="process-status">{pendingCount + ' pending'}</Label>}
        <DiagnosticBadge diagnostics={diagnostics} />
      </div>
      {shouldShow && (
        <div className="process-timeline">
          {parts.map((p, i) => {
            if (p.type === 'thinking') return <TimelineThinking key={'pt-' + i} content={p.content || ''} />;
            if (p.type === 'handoff') return <TimelineHandoff key={'pt-' + i} content={p.content || ''} />;
            if (p.type === 'tools') {
              return p.toolCalls.map(tc => (
                <ToolCallCard
                  key={tc.tool_call_id}
                  toolCall={tc}
                  live={live}
                  onApprove={onApprove}
                  onReject={onReject}
                  onInspectTask={onInspectTask}
                  onRetryTask={onRetryTask}
                  taskRetryable={retryableByCallId?.[tc.tool_call_id]}
                  liveTaskStatus={liveTaskStatusByCallId?.[tc.tool_call_id]}
                  liveTaskLabel={liveTaskLabelByCallId?.[tc.tool_call_id]}
                  taskLabelById={taskLabelById}
                />
              ));
            }
            return null;
          })}
          {live && reasoning && <TimelineThinking content={reasoning} />}
        </div>
      )}
    </div>
  );
}

// DiagnosticBadge reports trouble the run survived.
//
// It sits on the process group rather than in the transcript because that is
// what it describes: not something the agent said, but how the turn went. A run
// that answered after three retries or on a fallback model looks identical to
// one that answered first time, and this is the difference — the thing that
// explains why the answer took forty seconds, or why it is worse than usual.
function DiagnosticBadge({ diagnostics }: { diagnostics?: RunDiagnostic[] }) {
  if (!diagnostics || diagnostics.length === 0) return null;
  // Counted by kind: three retries is one fact, not three.
  const counts = new Map<string, number>();
  for (const d of diagnostics) counts.set(d.type, (counts.get(d.type) || 0) + 1);
  const summary = [...counts.entries()]
    .map(([type, n]) => (DIAGNOSTIC_LABELS[type] || type) + (n > 1 ? ' ×' + n : ''))
    .join(', ');
  const detail = diagnostics
    .map(d => (DIAGNOSTIC_LABELS[d.type] || d.type) + (d.message ? ': ' + d.message : ''))
    .join('\n');
  return (
    <Label variant="attention" className="process-status" title={detail}>{summary}</Label>
  );
}

function LiveTimer({ startedAt }: { startedAt: number }) {
  const [elapsed, setElapsed] = useState(() => Date.now() - startedAt);
  useEffect(() => {
    const id = setInterval(() => setElapsed(Date.now() - startedAt), 1000);
    return () => clearInterval(id);
  }, [startedAt]);
  return <span className="turn-duration turn-duration-live">{formatDuration(elapsed)}</span>;
}

interface TurnBlockProps {
  parts: TurnPart[];
  streaming: string | null;
  reasoning: string | null;
  isLive: boolean;
  liveAgentName?: string | null;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  // The entry id of the user message this turn answers — regenerating branches
  // back to it and runs again, which is why it is an ENTRY id and not a row id.
  regenMessageId?: string | null;
  regenContent?: string | null;
  onRegenerate?: (messageId: string, content: string) => void;
  running: boolean;
  compacting?: boolean;
  diagnostics?: RunDiagnostic[];
  duration?: string;
  liveStartedAt?: number | null;
  messageId?: string | number;
  // Sibling attempts at this point, and a switch to one of them.
  branches?: Branches;
  onSwitchBranch?: (tipEntryId: string) => void;
  onFork?: (id: string) => void;
  onInspectTask?: (taskId: string) => void;
  onRetryTask?: (taskId: string) => void;
  retryableByCallId?: Record<string, boolean>;
  liveTaskStatusByCallId?: Record<string, string>;
  liveTaskLabelByCallId?: Record<string, string>;
  taskLabelById?: Record<string, string>;
}

const TurnBlock = memo(function TurnBlock({ parts, streaming, reasoning, isLive, liveAgentName, onApprove, onReject, regenMessageId, regenContent, onRegenerate, running, compacting, diagnostics, duration, liveStartedAt, messageId, branches, onSwitchBranch, onFork, onInspectTask, onRetryTask, retryableByCallId, liveTaskStatusByCallId, liveTaskLabelByCallId, taskLabelById }: TurnBlockProps) {
  const isEmpty = parts.length === 0 && !streaming && !reasoning;
  const [copied, setCopied] = useState(false);

  const { segments, notices } = useMemo(() => buildSegments(parts), [parts]);

  // While live, the trailing process group is the one still executing — live
  // reasoning and the status label attach there; earlier groups have settled.
  // When the trailing segment is text (or the turn is empty) but reasoning is
  // already streaming, a tail group holds it until the next part arrives.
  const lastSeg = segments[segments.length - 1];
  const activeIdx = isLive && lastSeg?.kind === 'process' ? segments.length - 1 : -1;
  const liveTail = isLive && activeIdx === -1 && !!reasoning;

  const turnText = useMemo(
    () => parts.flatMap(p => p.type === 'text' ? [p.content] : []).join('\n\n'),
    [parts],
  );

  const handleCopy = useCallback(() => {
    if (!turnText) return;
    navigator.clipboard.writeText(turnText).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [turnText]);

  const canRegen = !!(regenMessageId && regenContent && onRegenerate);

  return (
    <div className="message message-turn">
      {segments.map((seg, i) =>
        seg.kind === 'text'
          ? <TextContent key={'seg-' + i} content={seg.content} />
          : <ProcessTimeline
              onInspectTask={onInspectTask}
              onRetryTask={onRetryTask}
              retryableByCallId={retryableByCallId}
              liveTaskStatusByCallId={liveTaskStatusByCallId}
              liveTaskLabelByCallId={liveTaskLabelByCallId}
              taskLabelById={taskLabelById}
              key={'seg-' + i}
              parts={seg.parts}
              live={i === activeIdx}
              reasoning={i === activeIdx ? reasoning : null}
              textStreaming={i === activeIdx && !!streaming}
              compacting={i === activeIdx ? compacting : false}
              diagnostics={i === activeIdx ? diagnostics : undefined}
              onApprove={onApprove}
              onReject={onReject}
            />
      )}
      {liveTail && (
        <ProcessTimeline
          onInspectTask={onInspectTask}
          onRetryTask={onRetryTask}
          retryableByCallId={retryableByCallId}
          liveTaskStatusByCallId={liveTaskStatusByCallId}
          liveTaskLabelByCallId={liveTaskLabelByCallId}
          taskLabelById={taskLabelById}
          parts={[]}
          live
          reasoning={reasoning}
          textStreaming={!!streaming}
          compacting={compacting}
          diagnostics={diagnostics}
          onApprove={onApprove}
          onReject={onReject}
        />
      )}
      {streaming && <StreamingMarkdown text={streaming} />}
      {notices.map((part, i) => (
        part.type === 'cancelled'
          ? <CancelledCard key={'notice-' + i} />
          : <ErrorCard key={'notice-' + i} message={part.content || 'Unknown error'} guardrail={part.guardrail} stage={part.stage} />
      ))}
      {isLive && isEmpty && !compacting && (
        <div className="thinking-indicator">
          <div className="thinking-dots">
            <span /><span /><span />
          </div>
          {liveAgentName && <span className="thinking-agent">{liveAgentName}</span>}
        </div>
      )}
      {isLive && compacting && activeIdx === -1 && !liveTail && (
        <div className="thinking-indicator">
          <div className="thinking-dots">
            <span /><span /><span />
          </div>
          <span className="thinking-agent">Compacting context…</span>
        </div>
      )}
      {isLive && liveStartedAt && <LiveTimer startedAt={liveStartedAt} />}
      {!isLive && turnText && (
        <div className="turn-actions">
          {branches && branches.tips.length > 1 && onSwitchBranch && (
            <span className="branch-switcher">
              <IconButton
                icon={ChevronLeftIcon}
                variant="invisible"
                size="small"
                aria-label="Previous attempt"
                disabled={branches.active === 0}
                onClick={() => onSwitchBranch(branches.tips[branches.active - 1])}
              />
              <span className="branch-count">{branches.active + 1} / {branches.tips.length}</span>
              <IconButton
                icon={ChevronRightIcon}
                variant="invisible"
                size="small"
                aria-label="Next attempt"
                disabled={branches.active >= branches.tips.length - 1}
                onClick={() => onSwitchBranch(branches.tips[branches.active + 1])}
              />
            </span>
          )}
          <IconButton
            icon={copied ? CheckIcon : CopyIcon}
            variant="invisible"
            size="small"
            aria-label={copied ? 'Copied!' : 'Copy'}
            onClick={handleCopy}
            style={copied ? { color: 'var(--fgColor-success)' } : undefined}
          />
          {messageId && onFork && (
            <IconButton
              icon={RepoForkedIcon}
              variant="invisible"
              size="small"
              aria-label="Fork"
              onClick={() => onFork(String(messageId))}
            />
          )}
          {!running && canRegen && (
            <IconButton
              icon={SyncIcon}
              variant="invisible"
              size="small"
              aria-label="Regenerate"
              onClick={() => onRegenerate!(regenMessageId!, regenContent!)}
            />
          )}
          {duration && <span className="turn-duration">{duration}</span>}
        </div>
      )}
    </div>
  );
});

// compactTokens renders an estimate the way an estimate should read: two
// significant figures and a k, never a precise-looking count. CharEstimator is
// a character ratio, not a tokenizer.
function compactTokens(n: number): string {
  return n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k' : String(n);
}

// CompactionCard is an inline marker where a pass happened: the history it
// folded renders in place ABOVE it (the transcript is decoupled from the
// model's context — see the Context panel for what the model still reads), so
// the card carries only the shrink figures and, one expand away, the summary
// that now stands in for that history in the model's view.
interface CompactionCardProps {
  content?: string;
  entryId?: string;
  tokensBefore?: number;
  tokensAfter?: number;
}

const CompactionCard = memo(function CompactionCard(
  { content, entryId, tokensBefore, tokensAfter }: CompactionCardProps,
) {
  const [expanded, setExpanded] = useState(false);
  const summaryText = (content || '').replace(/^\[Conversation Summary\]\s*/, '');
  const summaryHtml = useAsyncMarkdown(expanded ? summaryText : '');
  const shrank = tokensBefore && tokensAfter && tokensBefore > tokensAfter;

  return (
    // data-anchor-id: a checkpoint's summary can rank among the heaviest
    // items, and this marker is what that item jumps to.
    <div className="compaction-card" data-anchor-id={entryId || undefined}>
      <div
        className={'compaction-card-toggle' + (expanded ? ' expanded' : '')}
        onClick={() => setExpanded(!expanded)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>Compaction</span>
        {shrank && (
          <span className="compaction-card-savings">
            ~{compactTokens(tokensBefore)} → ~{compactTokens(tokensAfter)} tokens
          </span>
        )}
      </div>
      {expanded && (
        <div className="compaction-card-body">
          <div className="compaction-card-note">
            The history above stays in full — the model now reads this summary in its place.
          </div>
          {summaryText && <div className="markdown-body" dangerouslySetInnerHTML={{ __html: summaryHtml }} />}
        </div>
      )}
    </div>
  );
});

interface UserMessageProps {
  content: string;
  traceRunId?: string | null;
  onTrace?: (runId: string) => void;
  msgIdx: number;
  // The durable entry id, so the Context panel can scroll to this bubble.
  entryId?: string;
}

// Restartable jump-target flash, shared by trace reverse-navigation and the
// TOC rail.
function flashMessage(el: Element) {
  el.classList.remove('msg-jump-flash');
  // Restart the animation even when jumping to the same message twice.
  void (el as HTMLElement).offsetWidth;
  el.classList.add('msg-jump-flash');
  window.setTimeout(() => el.classList.remove('msg-jump-flash'), 1800);
}

const UserMessage = memo(function UserMessage({ content, traceRunId, onTrace, msgIdx, entryId }: UserMessageProps) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!content) return;
    navigator.clipboard.writeText(content).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [content]);

  // A server-injected notification (a finished task or workflow) never renders
  // in the timeline: the model reads it verbatim, but for the person the
  // composer's indicators and the Tasks panel are the surfaces — an in-flow
  // card duplicated them mid-conversation.
  if (parseTaskNotification(content)) return null;

  return (
    <div className="message message-user message-forkable" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx} data-anchor-id={entryId || undefined}>
      <div className="message-body">{content}</div>
      <div className="message-user-actions">
        {traceRunId && onTrace && (
          <IconButton
            icon={PulseIcon}
            variant="invisible"
            size="small"
            aria-label="Trace"
            onClick={() => onTrace(traceRunId)}
          />
        )}
        <IconButton
          icon={copied ? CheckIcon : CopyIcon}
          variant="invisible"
          size="small"
          aria-label={copied ? 'Copied!' : 'Copy'}
          onClick={handleCopy}
          style={copied ? { color: 'var(--fgColor-success)' } : undefined}
        />
      </div>
    </div>
  );
});

/* ---------- Greeting ---------- */

const GREETINGS: [number, string, string[]][] = [
  [6,  '🌌', ['Thoughts sharpen in the silence', 'The world sleeps, the mind wakes', 'Stillness is the seed of clarity']],
  [12, '🌅', ['A blank canvas, infinite paths', 'Begin before the doubts arrive', 'Morning knows what evening forgot']],
  [18, '☀️', ['Midway through, the view clears', 'Momentum hides in plain sight', 'The obstacle is the material']],
  [22, '🌇', ['The best work outlives the day', 'Dusk trades effort for insight', 'What you built today echoes tomorrow']],
];
const GREETING_NIGHT: [string, string[]] = ['🌙', ['One more thought before the stars', 'Night falls, ideas rise', 'The dark is just depth in disguise']];

function getGreeting(): [string, string] {
  const h = new Date().getHours();
  let emoji: string;
  let pool: string[];
  let matched = false;
  for (const [until, e, texts] of GREETINGS) {
    if (h < until) { emoji = e; pool = texts; matched = true; break; }
  }
  if (!matched) { emoji = GREETING_NIGHT[0]; pool = GREETING_NIGHT[1]; }
  return [emoji!, pool![Math.floor(Math.random() * pool!.length)]];
}

function Greeting() {
  // Pick once per mount. The call site keys this by session id, so the slogan
  // stays put across composer re-renders (e.g. switching the bottom agent /
  // sandbox picker) and only rerolls when a new or different session opens it.
  const [[emoji, text]] = useState(getGreeting);
  return (
    <div className="chat-greeting">
      <span className="chat-greeting-emoji">{emoji}</span>
      <span className="chat-greeting-text">{text}</span>
    </div>
  );
}

/* ---------- ChatView ---------- */

interface ChatViewProps {
  sessionId: string | null;
  // The session's display name for the top bar ('' until known).
  sessionName?: string;
  messages: ChatMessage[];
  // The session's raw entries, including the ones no longer on the active
  // branch. The rendered timeline drops those; the trace panel still lists
  // their runs, so it needs a source that has them.
  entries?: EntryView[];
  loaded: boolean;
  streaming: string | null;
  reasoning: string | null;
  running: boolean;
  compacting: boolean;
  // Trouble the live run survived, badged on the process group.
  diagnostics?: RunDiagnostic[];
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  liveAgentName: string | null;
  // The session is paused awaiting a tool approval: block new sends so the
  // approval is resolved first (a concurrent run would strand it as session_busy).
  awaiting?: boolean;
  // Background tasks spawned from this session (spawn_task), keyed by task id.
  tasks?: Record<string, TaskState>;
  // The Inspector's live view of the task being inspected (see useAgentSocket).
  taskView?: TaskViewState | null;
  onWatchTask?: (sid: string, taskId: string, childSessionId: string) => void;
  onUnwatchTask?: (sid: string) => void;
  // Applies a server-confirmed task state change (the stop API response) —
  // the fallback when no hub broadcast will come (paused task after restart).
  onPatchTask?: (sid: string, taskId: string, patch: Partial<TaskState>) => void;
  onSend: (text: string, agentConfigId: string, sandboxId: string, workDir?: string) => void;
  onCancel: (graceful?: boolean) => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onFork?: (id: string) => void;
  // Backwards pagination over the persisted history: hasMore says older
  // entries exist, onLoadEarlier fetches the previous page.
  hasMore?: boolean;
  loadingMore?: boolean;
  onLoadEarlier?: () => void;
  // Switches the session's active branch to another attempt.
  onSwitchBranch?: (tipEntryId: string) => void;
  // Forces one compaction pass now (the Context panel's button); resolves
  // after the timeline reload that follows a fold.
  onCompact?: () => Promise<void>;
  onRegenerate?: (userEntryId: string, userContent: string, agentConfigId: string, sandboxId: string, workDir?: string) => void;
  settingsReloadKey?: number;
  // Bumped by the app when the set of session bindings changed; refreshes the
  // Project picker's recent-projects aggregation.
  bindingsVersion?: number;
  // The session's permanent (sandbox, workdir) binding, or null while unbound.
  // Set by the first sandbox-carrying run; server-authoritative and immutable
  // afterwards — switching projects means starting a new session.
  sessionBinding?: SessionBinding | null;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  // Bumped by a workflow.updated event — refetches the strip and Tasks panel
  // when a background sequence moves (a step has no parent run to flip
  // `running`).
  workflowTick: number;
  // The composer's plan checkbox: seeded from the session, then this person's
  // intent until the next message carries it to the server.
  planning: boolean;
  onPlanningChange: (planning: boolean) => void;
  // Opens the global terminal panel (app-level, independent of the session).
  // Open-only by design: closing/collapsing happens on the panel itself. When
  // the session is bound to a terminal-capable sandbox its (sandbox, workdir)
  // is passed along, and a freshly opened panel starts a terminal for it —
  // in the same instance (and directory) the session's runs use.
  onTerminalOpen?: (sandbox?: { id: string; name: string; workDir?: string }) => void;
}

export function ChatView({
  sessionId, sessionName, messages, entries, loaded, streaming, reasoning, running, compacting, diagnostics,
  traceRuns, liveRunId, liveStartedAt, liveAgentName, awaiting, tasks, taskView,
  onWatchTask, onUnwatchTask, onPatchTask,
  workflowTick, planning, onPlanningChange,
  onSend, onCancel, onApprove, onReject, onFork, hasMore, loadingMore, onLoadEarlier, onSwitchBranch, onCompact, onRegenerate, settingsReloadKey, bindingsVersion,
  sessionBinding, panel, onPanelChange, onTerminalOpen,
}: ChatViewProps) {
  const [agentConfigId, setAgentConfigIdState] = useState(() => loadSessionAgent(sessionId || ''));
  const [sandboxId, setSandboxIdState] = useState(() => loadSessionSandbox(sessionId || ''));
  const [workDir, setWorkDirState] = useState(() => loadSessionWorkdir(sessionId || ''));
  // The "New project…" dialog: pick a sandbox, set its directory.
  const [projDialogOpen, setProjDialogOpen] = useState(false);
  const [projSandboxId, setProjSandboxId] = useState('');
  const [projPath, setProjPath] = useState('');

  useEffect(() => {
    setAgentConfigIdState(loadSessionAgent(sessionId || ''));
    setSandboxIdState(loadSessionSandbox(sessionId || ''));
    setWorkDirState(loadSessionWorkdir(sessionId || ''));
  }, [sessionId]);

  const setAgentConfigId = useCallback((id: string) => {
    setAgentConfigIdState(id);
    saveSessionAgent(sessionId || '', id);
  }, [sessionId]);

  const setWorkDir = useCallback((dir: string) => {
    setWorkDirState(dir);
    saveSessionWorkdir(sessionId || '', dir);
  }, [sessionId]);

  const setSandboxId = useCallback((id: string) => {
    setSandboxIdState(id);
    saveSessionSandbox(sessionId || '', id);
    // A custom path chosen for sandbox A must not silently apply to sandbox B.
    setWorkDirState('');
    saveSessionWorkdir(sessionId || '', '');
  }, [sessionId]);
  const [traceActiveRun, setTraceActiveRun] = useState<string | null>(null);
  // The span a Context panel jump asked the trace to open, and the counter that
  // makes repeating the same jump a fresh instruction.
  const [traceReveal, setTraceReveal] = useState<TraceReveal | null>(null);
  // A reveal is one instruction, not a standing state: once the trace panel
  // closes (or the session changes), a later manual open must not replay the
  // old jump's scroll.
  useEffect(() => {
    if (panel?.kind !== 'trace') setTraceReveal(null);
  }, [panel?.kind]);
  useEffect(() => {
    setTraceReveal(null);
  }, [sessionId]);
  const { data: agentConfigs, reload: reloadAgents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>);
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi<SandboxConfig[]>(() => api.sandboxes.list() as Promise<SandboxConfig[]>);
  // Bound sessions aggregated into the picker's "recent projects" — the same
  // hook the terminal panel's + menu uses.
  const projects = useRecentProjects(sandboxConfigs, bindingsVersion);

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    const valid = agentConfigs.some(a => a.id === agentConfigId);
    if (!valid) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs, agentConfigId, setAgentConfigId]);

  // A persisted sandbox may have since been deleted: drop a now-unknown id
  // back to None ('' is a valid choice), so the composer doesn't carry a stale
  // sandbox_id and the label doesn't fall back to a generic "Sandbox".
  useEffect(() => {
    if (!sandboxId || !sandboxConfigs) return;
    if (!sandboxConfigs.some(s => s.id === sandboxId)) setSandboxId('');
  }, [sandboxConfigs, sandboxId, setSandboxId]);

  useEffect(() => {
    if (settingsReloadKey) { reloadAgents(); reloadSandboxes(); }
  }, [settingsReloadKey, reloadAgents, reloadSandboxes]);

  // The dep must change on every content growth, not just on new messages:
  // .chat-messages opts out of native scroll anchoring, so streamed text and
  // reasoning deltas only keep the view pinned if they re-fire this effect.
  const { ref: scrollRef, isSticky, scrollToBottom } = useScrollToBottom(
    messages.length + (streaming?.length ?? 0) + (reasoning?.length ?? 0),
    sessionId,
  );
  // Plain element handle alongside the hook's callback ref — the TOC rail
  // needs the scroll container for active-item tracking and jump targets.
  const chatElRef = useRef<HTMLDivElement | null>(null);
  const composedScrollRef = useCallback((node: HTMLDivElement | null) => {
    chatElRef.current = node;
    scrollRef(node);
  }, [scrollRef]);

  const handleCopyClick = useCallback((e: MouseEvent<HTMLDivElement>) => {
    const expand = (e.target as HTMLElement).closest('.btn-code-expand') as HTMLElement | null;
    if (expand) {
      expand.closest('.code-block-wrapper')?.classList.remove('code-collapsed');
      return;
    }
    const btn = (e.target as HTMLElement).closest('.btn-copy') as HTMLElement | null;
    if (!btn) return;
    // getAttribute already returns the decoded value (the HTML parser resolved
    // the entities the renderer escaped into the attribute). A second manual
    // decode here corrupted any code that literally contained an entity like
    // "&amp;", so read the attribute as-is.
    const code = btn.getAttribute('data-code');
    if (!code) return;
    navigator.clipboard.writeText(code).then(() => {
      btn.classList.add('copied');
      const svgContent = btn.innerHTML;
      btn.innerHTML = CHECK_ICON;
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svgContent; }, 1500);
    });
  }, []);

  const selectedSandbox = sandboxConfigs?.find(s => s.id === sandboxId);
  const sandboxView = composerSandboxView(sessionBinding || null, selectedSandbox, sandboxConfigs, workDir);

  const handleSend = useCallback((text: string) => {
    // No sessionId is fine: sending with no active session starts a new session
    // (app-level onSend auto-creates it). Only an agent is required.
    if (!agentConfigId) return;
    if (sessionBinding) {
      // Bound: the server uses the binding regardless — send no sandbox claim.
      onSend(text, agentConfigId, '', '');
      return;
    }
    // The workdir that would bind is the view's effectiveWorkDir — the same
    // value the picker button and the dialog show, snapshotted explicitly so
    // the binding does not drift with later config edits. One validation
    // source (bindingWorkDirIssue) guards it, mirroring the server's rules.
    const sel = sandboxConfigs?.find(s => s.id === sandboxId);
    const issue = bindingWorkDirIssue(sel, workDir);
    if (issue) {
      toast.error(issue);
      return;
    }
    onSend(text, agentConfigId, sandboxId, sandboxView.effectiveWorkDir);
  }, [agentConfigId, sandboxId, workDir, sandboxConfigs, sessionBinding, sandboxView.effectiveWorkDir, onSend]);


  const handleCancel = useCallback((graceful?: boolean) => {
    onCancel(graceful);
    toast.info(graceful ? 'Stopping after the current turn…' : 'Run cancelled');
  }, [onCancel]);

  // A wake run's input is the raw notification text — label it by the task,
  // phrased so it reads as the parent's reaction to the result, not as the
  // task's own trace (the task's trace lives in the Inspector).
  const runLabelFor = (content: string) => {
    const notif = parseTaskNotification(content);
    if (!notif) return content;
    const labels = notif.items.map(it => it.label).filter(Boolean);
    const which = labels.length > 1 ? labels.join(', ') : (labels[0] || notif.taskId || '');
    // A workflow's notification has no Task lines to parse: its first line
    // already names the sequence and says how it ended.
    if (!which) return notif.text.split('\n')[0];
    return 'task result: ' + which;
  };

  const { turnRunMap, userRunMap, runLabels, staleRuns } = useMemo(() => {
    const tMap: Record<number, string> = {};
    const uMap: Record<number, string> = {};
    const labels: Record<string, string> = {};
    let turnIdx = 0;
    for (let i = 0; i < messages.length; i++) {
      const entry = messages[i] as any;
      const rid = entry.runId;
      // Label runs from the user message directly, so a run whose reply
      // produced no visible turn still shows its question in the trace panel.
      if (entry.role === 'user' && rid && traceRuns[rid]) {
        // Notifications don't render, so they anchor no jump target — label
        // the run but keep it out of userRunMap/messageRunIds.
        if (!parseTaskNotification(entry.content)) uMap[i] = rid;
        if (entry.content && !labels[rid]) labels[rid] = runLabelFor(entry.content);
      } else if (entry.role === 'turn') {
        if (rid && traceRuns[rid]) {
          tMap[i] = rid;
          let userContent: string | null = null;
          for (let j = i - 1; j >= 0; j--) {
            if (messages[j].role === 'user') {
              userContent = messages[j].content ?? null;
              // The turn's run OVERWRITES the one the user message carries.
              // A message's own run_id is whichever run first produced it —
              // after a regenerate that is an attempt the session has since
              // branched away from, and it would claim the jump target for a
              // run no longer in the timeline while the current attempt got
              // none. On the active branch a message is followed by exactly
              // one turn, so there is nothing to contend over.
              if (!parseTaskNotification(messages[j].content)) uMap[j] = rid;
              break;
            }
          }
          if (!labels[rid]) {
            labels[rid] = userContent ? runLabelFor(userContent) : 'Turn ' + (turnIdx + 1);
          }
        }
        turnIdx++;
      }
    }
    // Runs whose turn is NOT in the rendered timeline: a regenerated answer
    // the session has since branched away from. Their traces are still listed
    // — the work happened — but the timeline has no turn to label them from,
    // so they fell back to a raw run id. Label them from the entries instead,
    // and mark them, so "5 traces, 3 exchanges" reads as what it is rather
    // than as a mismatch.
    const stale = new Set<string>();
    let lastUser: string | null = null;
    for (const e of entries || []) {
      if (e.role === 'user' && e.content) lastUser = e.content;
      const rid = e.run_id;
      if (!rid || !traceRuns[rid]) continue;
      if (e.on_path === false) stale.add(rid);
      if (!labels[rid] && lastUser) labels[rid] = runLabelFor(lastUser);
    }
    return { turnRunMap: tMap, userRunMap: uMap, runLabels: labels, staleRuns: stale };
  }, [messages, entries, traceRuns]);

  // Wake-up run → the run whose spawn_task started the chain, read straight
  // off the trace: a wake run's spans carry parent_run_id, recorded at launch.
  // The lineage lives on the run's own durable output — deriving it here from
  // task rows and notification text broke on every surface that does not carry
  // them (a fork copies traces but not task rows; a fold moves the
  // notification out of the rendered timeline).
  const traceRunParents = useMemo(() => {
    const parents: Record<string, string> = {};
    for (const [rid, evs] of Object.entries(traceRuns)) {
      for (const ev of evs) {
        if (ev.parent_run_id && ev.parent_run_id !== rid) {
          parents[rid] = ev.parent_run_id;
          break;
        }
      }
    }
    return parents;
  }, [traceRuns]);

  const openTrace = useCallback((runId: string) => {
    onPanelChange({ kind: 'trace' });
    setTraceActiveRun(runId);
  }, [onPanelChange]);

  const openTaskDetail = useCallback((taskId: string) => {
    onPanelChange({ kind: 'task', taskId });
  }, [onPanelChange]);

  // The session's workflow executions, and the decisions its background work is
  // waiting on. A step runs in a hidden child session, so the parent's `running`
  // does NOT flip when one starts or pauses — workflowTick (a workflow.updated
  // nudge from that step's run) is what carries a mid-sequence move here; the
  // `running` flip still covers the start_workflow turn and the wake-up.
  const { data: workflowRuns, reload: reloadWorkflowRuns } = useApi<WorkflowRunRow[]>(
    () => (sessionId ? api.sessions.workflowRuns(sessionId) : Promise.resolve([])) as Promise<WorkflowRunRow[]>,
    [sessionId, running, workflowTick],
  );
  const { data: backgroundApprovals } = useApi<SessionApproval[]>(
    () => (sessionId ? api.sessions.approvals(sessionId) : Promise.resolve([])) as Promise<SessionApproval[]>,
    [sessionId, running, workflowTick],
  );
  // Tasks and workflow executions as one list: the strip above the composer,
  // the Tasks panel and the top bar's gate all read it. The runs are filtered
  // to this session: useApi is a single slot, so during a switch it still
  // holds the previous session's rows (with live Approve buttons). Approvals
  // only surface through a matching run, so filtering runs covers both.
  const backgroundItems = useMemo(
    () => mergeBackground(tasks, (workflowRuns || []).filter(wr => wr.parent_session_id === sessionId), backgroundApprovals),
    [tasks, workflowRuns, backgroundApprovals, sessionId],
  );
  const inspectedItem = panel?.kind === 'task' ? backgroundItems.find(it => it.id === panel.taskId) : undefined;

  // Stop and retry dispatch on the kind — different endpoints are the only
  // thing the merged list still has to tell the two apart for.
  const stopWork = useCallback(async (item: BackgroundItem) => {
    try {
      if (item.kind === 'workflow') {
        await api.workflowRuns.stop(item.id);
        reloadWorkflowRuns();
        return;
      }
      const info = await (api.tasks.stop(item.id) as Promise<{ status?: string }>);
      // Apply the confirmed state directly: after a restart the hub has no
      // record of the run, so no run.cancelled broadcast will arrive.
      if (sessionId && info?.status) {
        onPatchTask?.(sessionId, item.id, { status: info.status as TaskStatus, pendingCallId: undefined, pendingToolName: undefined });
      }
    } catch (e) {
      toast.error((e as Error).message || 'Stop failed');
    }
  }, [sessionId, onPatchTask, reloadWorkflowRuns]);

  // By id, because the spawn card in the transcript has a task and nothing else.
  const retryTask = useCallback(async (taskId: string) => {
    try {
      const info = await (api.tasks.retry(taskId) as Promise<{ status?: string; attempt?: number; max_attempts?: number }>);
      // The confirmed state, applied without waiting for the broadcast — the
      // same reason stopWork does: the answer is already in hand, and a button
      // that stays on "failed" invites a second click that will be refused. The
      // failed attempt's summary goes with it.
      if (sessionId && info?.status) {
        onPatchTask?.(sessionId, taskId, {
          status: info.status as TaskStatus, attempt: info.attempt,
          maxAttempts: info.max_attempts, summary: undefined,
        });
      }
    } catch (e) {
      toast.error((e as Error).message || 'Retry failed');
    }
  }, [sessionId, onPatchTask]);

  const retryWork = useCallback(async (item: BackgroundItem) => {
    if (item.kind === 'task') return retryTask(item.id);
    try {
      await api.workflowRuns.retry(item.id);
      reloadWorkflowRuns();
    } catch (e) {
      toast.error((e as Error).message || 'Retry failed');
    }
  }, [retryTask, reloadWorkflowRuns]);

  // Workflow only: hides the failed bar without giving up the row (the Tasks
  // panel keeps it, and a retry un-dismisses server-side).
  const dismissWork = useCallback(async (item: BackgroundItem) => {
    try {
      await api.workflowRuns.dismiss(item.id);
      reloadWorkflowRuns();
    } catch (e) {
      toast.error((e as Error).message || 'Dismiss failed');
    }
  }, [reloadWorkflowRuns]);

  // The detail lens is live: tell the socket layer which child session to tail.
  const inspectedChild = inspectedItem?.childSessionId;
  const inspectedId = inspectedItem?.id;
  useEffect(() => {
    if (!sessionId || !inspectedId || !inspectedChild || !onWatchTask || !onUnwatchTask) return;
    onWatchTask(sessionId, inspectedId, inspectedChild);
    return () => onUnwatchTask(sessionId);
  }, [sessionId, inspectedId, inspectedChild, onWatchTask, onUnwatchTask]);

  // Runs that have a user message in this conversation — gates the trace
  // panel's jump-to-message control.
  // Runs the trace panel can offer a "jump to message" for: those with
  // something in the RENDERED timeline to jump to. A run whose attempt was
  // regenerated away has no anchor — the jump would scroll to nothing — so it
  // gets no button, which is also what distinguishes it from the attempt that
  // replaced it.
  const messageRunIds = useMemo(
    () => new Set([...Object.values(userRunMap), ...Object.values(turnRunMap)]),
    [userRunMap, turnRunMap],
  );

  // Reverse navigation: scroll the chat to the run's user message and flash
  // it, mirroring the message → trace direction of openTrace.
  const jumpToRun = useCallback((runId: string) => {
    const el = document.querySelector(`.chat-messages [data-run-id="${CSS.escape(runId)}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    flashMessage(el);
  }, []);

  // TOC rail: one entry per user prompt; click scrolls to the message. The
  // upward smooth scroll trips the scroll hook's moved-up intent detection,
  // so following pauses automatically while the user reads.
  // toolCallId → live task status, for the spawn card badge. Status-only and
  // identity-stable across unrelated task events (lastTool etc.), so the
  // memoized TurnBlocks only re-render on real transitions.
  const liveTaskStatusRef = useRef<Record<string, string>>({});
  const liveTaskStatusByCallId = useMemo(() => {
    const next: Record<string, string> = {};
    for (const t of Object.values(tasks || {})) {
      if (t.toolCallId && (t.status === 'working' || t.status === 'input_required')) next[t.toolCallId] = t.status;
    }
    const prev = liveTaskStatusRef.current;
    const same = Object.keys(next).length === Object.keys(prev).length &&
      Object.entries(next).every(([k, v]) => prev[k] === v);
    if (!same) liveTaskStatusRef.current = next;
    return liveTaskStatusRef.current;
  }, [tasks]);

  // toolCallId → whether the server would accept a retry of that task. The
  // card cannot work it out: "failed" says nothing about attempts left.
  const retryableByCallId = useMemo(() => {
    const next: Record<string, boolean> = {};
    for (const t of Object.values(tasks || {})) {
      if (t.toolCallId && taskRetryable(t)) next[t.toolCallId] = true;
    }
    return next;
  }, [tasks]);

  // toolCallId → live task title (the spawn label), for the spawn card header
  // before the terminal display projection lands. Identity-stable like the
  // status map above so the memoized TurnBlocks don't churn on unrelated events.
  const liveTaskLabelRef = useRef<Record<string, string>>({});
  const liveTaskLabelByCallId = useMemo(() => {
    const next: Record<string, string> = {};
    for (const t of Object.values(tasks || {})) {
      if (t.toolCallId && t.label) next[t.toolCallId] = t.label;
    }
    const prev = liveTaskLabelRef.current;
    const same = Object.keys(next).length === Object.keys(prev).length &&
      Object.entries(next).every(([k, v]) => prev[k] === v);
    if (!same) liveTaskLabelRef.current = next;
    return liveTaskLabelRef.current;
  }, [tasks]);

  // taskId → label, so task_status / task_stop cards resolve the readable title
  // of the task they reference. Same identity-stability as the maps above.
  const taskLabelByIdRef = useRef<Record<string, string>>({});
  const taskLabelById = useMemo(() => {
    const next: Record<string, string> = {};
    for (const t of Object.values(tasks || {})) {
      if (t.taskId && t.label) next[t.taskId] = t.label;
    }
    const prev = taskLabelByIdRef.current;
    const same = Object.keys(next).length === Object.keys(prev).length &&
      Object.entries(next).every(([k, v]) => prev[k] === v);
    if (!same) taskLabelByIdRef.current = next;
    return taskLabelByIdRef.current;
  }, [tasks]);

  const tocItems = useMemo(() =>
    messages.flatMap((m, i) => m.role === 'user' && m.content && !parseTaskNotification(m.content)
      ? [{ idx: i, preview: m.content.replace(/\s+/g, ' ').trim().slice(0, 60) }]
      : []),
    [messages]);

  const jumpToMsg = useCallback((idx: number) => {
    const el = chatElRef.current?.querySelector(`[data-msg-idx="${idx}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'start' });
    flashMessage(el);
  }, []);

  // Keep agent/sandbox selection in a ref so the regenerate callback stays
  // referentially stable — a new closure per render would defeat TurnBlock's memo.
  // Bound sessions claim no sandbox (the server uses the binding); unbound ones
  // carry the choice, because a regen can be the first sandbox-carrying run.
  const regenConfigRef = useRef({ agentConfigId, sandboxId, workDir: '' });
  regenConfigRef.current = sessionBinding
    ? { agentConfigId, sandboxId: '', workDir: '' }
    : { agentConfigId, sandboxId, workDir: sandboxView.effectiveWorkDir };
  const handleRegen = useCallback((messageId: string, content: string) => {
    if (!onRegenerate) return;
    onRegenerate(messageId, content, regenConfigRef.current.agentConfigId, regenConfigRef.current.sandboxId, regenConfigRef.current.workDir);
  }, [onRegenerate]);

  const topBar = (
    <ChatTopBar
      sessionName={sessionName || ''}
      sessionId={sessionId}
      panel={panel}
      onPanelChange={onPanelChange}
      backgroundCount={backgroundItems.length}
      terminalEnabled={!!onTerminalOpen && !!sandboxConfigs?.some(s => s.terminal)}
      onTerminalOpen={onTerminalOpen
        ? () => {
          // A bound session's terminal follows its binding — same sandbox
          // instance, same working directory as the runs. Unbound sessions
          // fall back to the picker's current (uncommitted) selection.
          const bound = sessionBinding ? sandboxConfigs?.find(s => s.id === sessionBinding.sandboxId) : undefined;
          if (bound?.terminal) {
            onTerminalOpen({ id: bound.id, name: bound.name, workDir: sessionBinding?.workDir || undefined });
          } else if (!sessionBinding && selectedSandbox?.terminal) {
            onTerminalOpen({ id: selectedSandbox.id, name: selectedSandbox.name });
          } else {
            onTerminalOpen(undefined);
          }
        }
        : undefined}
      binding={sandboxView.bound && sessionBinding
        ? { title: sandboxView.title, workDir: sessionBinding.workDir }
        : null}
    />
  );

  if (!sessionId) {
    return (
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
          <div className="chat-empty">
            <Blankslate>
              <Blankslate.Visual>
                <CommentDiscussionIcon size={24} />
              </Blankslate.Visual>
              <Blankslate.Heading>Start a conversation</Blankslate.Heading>
              <Blankslate.Description>Pick a chat from the sidebar, or create a new one to begin.</Blankslate.Description>
            </Blankslate>
          </div>
        </div>
      </div>
    );
  }

  const selectedAgentLabel = agentConfigs?.find(a => a.id === agentConfigId)?.name || 'Agent';

  const inputToolbar: ReactNode = (
    <>
      <div className="chat-input-toolbar-left">
        <IconButton
          icon={PlusIcon}
          variant="invisible"
          size="small"
          aria-label="Add context"
          disabled
        />
        <SlashMenu planning={planning} onChange={onPlanningChange} running={running} />
        {/* Bound sessions show nothing here — the binding lives in the top
            bar's badge. Before binding the picker offers PROJECTS — recent
            (directory, sandbox) pairs aggregated from bound sessions — because
            the directory is what a person recognizes; the backend is its
            attribute, not the other way around. */}
        {!sandboxView.bound && sandboxConfigs && sandboxConfigs.length > 0 && (
          <ActionMenu>
            <ActionMenu.Button size="small" variant="invisible" leadingVisual={FileDirectoryIcon}>
              {sandboxId && selectedSandbox ? projectLabel(sandboxView.effectiveWorkDir, selectedSandbox.name) : 'Project'}
            </ActionMenu.Button>
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                <ActionList.Item selected={sandboxId === ''} onSelect={() => setSandboxId('')}>
                  None
                  <ActionList.Description variant="inline">chat only</ActionList.Description>
                </ActionList.Item>
                {/* One group per sandbox: the group heading carries the
                    backend, rows carry just the project name — the full path
                    lives in the hover title. */}
                {groupProjects(projects).map(g => (
                  <ActionList.Group key={g.sandboxId}>
                    {/* Primer requires an explicit heading level on list-role
                        ActionLists; omitting `as` throws and unmounts the app. */}
                    <ActionList.GroupHeading as="h3">{g.sandboxName}</ActionList.GroupHeading>
                    {g.items.map(p => (
                      <ActionList.Item
                        key={p.sandboxId + ' ' + p.workDir}
                        selected={sandboxId === p.sandboxId && sandboxView.effectiveWorkDir === p.workDir}
                        onSelect={() => { setSandboxId(p.sandboxId); setWorkDir(p.workDir); }}
                        title={p.title}
                      >
                        {p.base}
                      </ActionList.Item>
                    ))}
                  </ActionList.Group>
                ))}
                <ActionList.Divider />
                <ActionList.Item
                  onSelect={() => {
                    const initial = selectedSandbox || sandboxConfigs[0];
                    setProjSandboxId(initial.id);
                    setProjPath(sandboxId && selectedSandbox ? sandboxView.effectiveWorkDir : (initial.default_work_dir || ''));
                    setProjDialogOpen(true);
                  }}
                >
                  New project…
                </ActionList.Item>
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        )}
        {projDialogOpen && (() => {
          const projSandbox = sandboxConfigs?.find(s => s.id === projSandboxId);
          const editable = !!projSandbox?.work_dir_editable;
          const isDocker = projSandbox?.type === 'docker';
          const isSSH = projSandbox?.type === 'ssh';
          // One validation source with the send-time guard: the dialog must
          // not accept a draft that sending will refuse.
          const pathIssue = bindingWorkDirIssue(projSandbox, projPath);
          const pathValid = !pathIssue;
          return (
            <Dialog
              title="New project"
              onClose={() => setProjDialogOpen(false)}
              width="large"
              footerButtons={[
                { content: 'Cancel', onClick: () => setProjDialogOpen(false) },
                {
                  content: 'Select',
                  buttonType: 'primary',
                  disabled: !projSandbox || !pathValid,
                  onClick: () => {
                    if (!projSandbox || !pathValid) return;
                    setSandboxId(projSandbox.id);
                    // A non-editable backend stores no workdir draft: its
                    // directory is fixed, and a snapshot of it would be sent
                    // as a directory claim the server refuses.
                    setWorkDir(editable ? projPath.trim() : '');
                    setProjDialogOpen(false);
                  },
                },
              ]}
            >
              <Stack gap="normal">
                {fc('Sandbox', (
                  <Select
                    block
                    value={projSandboxId}
                    onChange={e => {
                      const id = e.target.value;
                      setProjSandboxId(id);
                      setProjPath(sandboxConfigs?.find(s => s.id === id)?.default_work_dir || '');
                    }}
                  >
                    {sandboxConfigs?.map(s => (
                      <Select.Option key={s.id} value={s.id}>{s.name}</Select.Option>
                    ))}
                  </Select>
                ), '')}
                {fc('Directory', (
                  <TextInput
                    block
                    value={projPath}
                    disabled={!editable}
                    validationStatus={pathValid ? undefined : 'error'}
                    placeholder={editable ? (projSandbox?.default_work_dir || '(sandbox default)') : undefined}
                    onChange={e => setProjPath(e.target.value)}
                  />
                ), !editable
                  ? 'An ephemeral docker container always runs in /workspace.'
                  : isDocker
                    ? 'Must be /workspace or a subdirectory of it.'
                    : isSSH
                      ? (projSandbox?.default_work_dir
                        ? 'An absolute remote path; empty = the sandbox\'s default directory.'
                        : 'Required: an absolute remote directory keeps the session\'s files between commands.')
                      : (projSandbox?.default_work_dir
                        ? 'Empty = the sandbox\'s default directory.'
                        : 'Empty = the server workspace directory.'))}
              </Stack>
            </Dialog>
          );
        })()}
      </div>
      <div className="chat-input-toolbar-right">
        {agentConfigs && agentConfigs.length > 0 ? (
          <ActionMenu>
            <ActionMenu.Button size="small" variant="invisible" leadingVisual={DependabotIcon}>
              {selectedAgentLabel}
            </ActionMenu.Button>
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                {agentConfigs.map(a => (
                  <ActionList.Item key={a.id} selected={agentConfigId === a.id} onSelect={() => setAgentConfigId(a.id)}>
                    {a.name}
                  </ActionList.Item>
                ))}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        ) : (
          <span className="chat-input-toolbar-warn">No agents — go to Settings</span>
        )}
      </div>
    </>
  );

  // One transcript row. Extracted from the render so the workflow grouping can
  // wrap a span of them without the map body moving.
  const renderMessage = (m: ChatMessage, i: number) => {
    if (m.role === 'turn') {
        const isLive = running && i === messages.length - 1;
        let prevUserContent: string | null = null;
        let prevUserEntryId: string | undefined;
        for (let j = i - 1; j >= 0; j--) {
          if (messages[j].role === 'user') {
            prevUserContent = messages[j].content ?? null;
            prevUserEntryId = messages[j].entryId;
            break;
          }
        }
        const rid = turnRunMap[i];
        const turnDuration = rid && traceRuns[rid]
          ? traceRuns[rid].find(e => e.kind === 'span' && e.duration)?.duration
          : undefined;
        return (
          <TurnBlock
            key={entryKey(m, i, 'turn')}
            onInspectTask={openTaskDetail}
            onRetryTask={retryTask}
            retryableByCallId={retryableByCallId}
            parts={m.parts || []}
            streaming={isLive ? streaming : null}
            reasoning={isLive ? reasoning : null}
            isLive={isLive}
            liveAgentName={isLive ? liveAgentName : null}
            onApprove={onApprove}
            onReject={onReject}
            regenMessageId={prevUserEntryId || null}
            regenContent={prevUserContent}
            onRegenerate={onRegenerate ? handleRegen : undefined}
            running={running}
            compacting={isLive ? compacting : false}
            diagnostics={isLive ? diagnostics : undefined}
            duration={turnDuration}
            liveStartedAt={isLive ? liveStartedAt : undefined}
            messageId={m.messageId}
            branches={m.branches}
            onSwitchBranch={onSwitchBranch}
            onFork={onFork}
            liveTaskStatusByCallId={liveTaskStatusByCallId}
            liveTaskLabelByCallId={liveTaskLabelByCallId}
            taskLabelById={taskLabelById}
          />
        );
      }
      if (m.role === 'user') {
        const rid = userRunMap[i];
        return (
          <UserMessage
            key={entryKey(m, i, 'user')}
            content={m.content || ''}
            traceRunId={rid || null}
            onTrace={openTrace}
            msgIdx={i}
            entryId={m.entryId}
          />
        );
      }
      if (m.role === 'compaction') {
        return <CompactionCard key={entryKey(m, i, 'compaction')} {...m} />;
      }
      return <MessageBubble key={entryKey(m, i, 'msg')} role={m.role} content={m.content || ''} />;
  };

  const isEmpty = loaded && messages.length === 0;

  if (!loaded && messages.length === 0) {
    return (
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
        </div>
      </div>
    );
  }

  if (isEmpty) {
    return (
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
          <div className="chat-content chat-content-centered">
            <Greeting key={`greeting-${sessionId}`} />
            <WorkflowStrip items={backgroundItems} onOpen={openTaskDetail} onApprove={onApprove} onReject={onReject} onStop={stopWork} onRetry={retryWork} onDismiss={dismissWork} />
            <MessageInput
              key={`input-${sessionId}`}
              sessionId={sessionId}
              onSend={handleSend}
              onCancel={handleCancel}
              disabled={running || awaiting || !agentConfigId}
              running={running}
              toolbar={inputToolbar}
            />
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className={'chat-main' + (panel ? ' trace-open' : '')}>
      <div className="chat-content">
        {topBar}
        <div className="chat-messages-area">
        <div ref={composedScrollRef} className="chat-messages" onClick={handleCopyClick}>
          {hasMore && (
            <div className="load-earlier">
              <Button size="small" variant="invisible" onClick={onLoadEarlier} disabled={loadingMore}>
                {loadingMore ? 'Loading…' : 'Load earlier messages'}
              </Button>
            </div>
          )}
          {messages.map(renderMessage)}
        </div>

        {!isSticky && (
          <Button size="small" leadingVisual={ArrowDownIcon} className="scroll-to-bottom" onClick={scrollToBottom}>
            {streaming ? 'Responding…' : 'Jump to latest'}
          </Button>
        )}
        <ChatToc items={tocItems} scrollElRef={chatElRef} onJump={jumpToMsg} />
        </div>
        <WorkflowStrip items={backgroundItems} onOpen={openTaskDetail} onApprove={onApprove} onReject={onReject} onStop={stopWork} onRetry={retryWork} onDismiss={dismissWork} />
        <MessageInput
          key={`input-${sessionId}`}
          sessionId={sessionId}
          onSend={handleSend}
          onCancel={handleCancel}
          disabled={running || awaiting || !agentConfigId}
          running={running}
          toolbar={inputToolbar}
        />
      </div>

      {panel?.kind === 'trace' && (
        <TraceDrawer
          traceRuns={traceRuns}
          liveRunId={liveRunId}
          activeRunId={traceActiveRun}
          runLabels={runLabels}
          staleRuns={staleRuns}
          runParents={traceRunParents}
          onClose={() => onPanelChange(null)}
          onJumpToRun={jumpToRun}
          messageRunIds={messageRunIds}
          reveal={traceReveal || undefined}
        />
      )}
      {panel?.kind === 'context' && sessionId && (
        <ContextPanel
          sessionId={sessionId}
          running={running}
          // entries is replaced by every server re-read of the timeline — a
          // branch switch included, which running alone would miss.
          reloadKey={entries}
          onClose={() => onPanelChange(null)}
          onCompact={onCompact}
        />
      )}
      {panel?.kind === 'tasks' && (
        <BackgroundListPanel
          items={backgroundItems}
          onOpen={openTaskDetail}
          onClose={() => onPanelChange(null)}
          onApprove={onApprove}
          onReject={onReject}
          onStop={stopWork}
          onRetry={retryWork}
        />
      )}
      {panel?.kind === 'task' && inspectedItem && (
        <BackgroundDetailPanel
          item={inspectedItem}
          view={taskView && taskView.taskId === inspectedItem.id ? taskView : null}
          onBack={() => onPanelChange({ kind: 'tasks' })}
          onClose={() => onPanelChange(null)}
          onApprove={onApprove}
          onReject={onReject}
          onStop={stopWork}
          onRetry={retryWork}
        />
      )}
    </div>
  );
}
