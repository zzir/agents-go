import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, memo, type MouseEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Button, IconButton, Label, ActionMenu, ActionList } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useAsyncMarkdown, splitMermaidBlocks, sanitizeSVG } from '@/lib/markdown';
import { CHECK_ICON } from '@/lib/markdownShared';
import { formatDuration, type TurnPart, type ErrorPart, type CancelledPart } from '@/lib/timeline';
import { useScrollToBottom, useApi } from '@/lib/hooks';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { MessageInput } from '@/features/chat/MessageInput';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { TraceDrawer, type TraceEventData } from '@/features/chat/TracePanel';
import { ArrowDownIcon, ArrowSwitchIcon, ChevronRightIcon, RepoForkedIcon, CopyIcon, CheckIcon, SyncIcon, CommentDiscussionIcon, PulseIcon, PlusIcon, ContainerIcon, DependabotIcon, CodeIcon, EyeIcon, AlertIcon, LightBulbIcon, StopIcon, ShieldIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { toast } from '@/lib/toast';

/* ---------- types ---------- */

interface ChatMessage {
  role: string;
  content?: string;
  messageId?: string;
  parts?: TurnPart[];
}


interface AgentConfig {
  id: string;
  name: string;
}

interface SandboxConfig {
  id: string;
  name: string;
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
        const decoded = source.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
        const { svg: rendered } = await mermaid.render(id, decoded);
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

function ErrorCard({ message, guardrail, stage }: { message: string; guardrail?: string; stage?: string }) {
  // A guardrail block is not a system failure — render it as a distinct
  // "blocked" state. An output-stage trip means the answer already streamed and
  // was retracted, so say so.
  if (guardrail) {
    const label = `Blocked by guardrail “${guardrail}”`;
    const note = stage === 'output'
      ? 'The response above was blocked before delivery.'
      : 'The request was blocked before the model ran.';
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
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
}

// One collapsible group of thinking + tool-call parts. `live` marks the group
// still executing (the trailing one while its run is live): it stays open and
// shows a status label; settled groups collapse to "N steps".
function ProcessTimeline({ parts, live, reasoning, textStreaming, compacting, onApprove, onReject }: ProcessTimelineProps) {
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
    <div className="process-group">
      <div
        className={'process-group-toggle' + (shouldShow ? ' expanded' : '')}
        onClick={() => setExpanded(!shouldShow)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>{label}</span>
        {pendingCount > 0 && <Label variant="accent" className="process-status">{pendingCount + ' pending'}</Label>}
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
  regenMessageId?: string | null;
  regenContent?: string | null;
  onRegenerate?: (messageId: string, content: string) => void;
  running: boolean;
  compacting?: boolean;
  duration?: string;
  liveStartedAt?: number | null;
  messageId?: string | number;
  onFork?: (id: string) => void;
}

const TurnBlock = memo(function TurnBlock({ parts, streaming, reasoning, isLive, liveAgentName, onApprove, onReject, regenMessageId, regenContent, onRegenerate, running, compacting, duration, liveStartedAt, messageId, onFork }: TurnBlockProps) {
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
              key={'seg-' + i}
              parts={seg.parts}
              live={i === activeIdx}
              reasoning={i === activeIdx ? reasoning : null}
              textStreaming={i === activeIdx && !!streaming}
              compacting={i === activeIdx ? compacting : false}
              onApprove={onApprove}
              onReject={onReject}
            />
      )}
      {liveTail && (
        <ProcessTimeline
          parts={[]}
          live
          reasoning={reasoning}
          textStreaming={!!streaming}
          compacting={compacting}
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

const CompactionCard = memo(function CompactionCard({ content }: { content: string }) {
  const [expanded, setExpanded] = useState(false);
  const summaryText = content.replace(/^\[Conversation Summary\]\s*/, '');
  const summaryHtml = useAsyncMarkdown(expanded ? summaryText : '');

  return (
    <div className="compaction-card">
      <div
        className={'compaction-card-toggle' + (expanded ? ' expanded' : '')}
        onClick={() => setExpanded(!expanded)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>Compaction</span>
        <Label variant="attention" className="process-status">summarized</Label>
      </div>
      {expanded && (
        <div className="compaction-card-body markdown-body" dangerouslySetInnerHTML={{ __html: summaryHtml }} />
      )}
    </div>
  );
});

interface UserMessageProps {
  content: string;
  traceRunId?: string | null;
  onTrace?: (runId: string) => void;
}

const UserMessage = memo(function UserMessage({ content, traceRunId, onTrace }: UserMessageProps) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!content) return;
    navigator.clipboard.writeText(content).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [content]);

  return (
    <div className="message message-user message-forkable" data-run-id={traceRunId || undefined}>
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
  const [emoji, text] = getGreeting();
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
  messages: ChatMessage[];
  loaded: boolean;
  streaming: string | null;
  reasoning: string | null;
  running: boolean;
  compacting: boolean;
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  liveAgentName: string | null;
  // The session is paused awaiting a tool approval: block new sends so the
  // approval is resolved first (a concurrent run would strand it as session_busy).
  awaiting?: boolean;
  onSend: (text: string, agentConfigId: string, sandboxId: string) => void;
  onCancel: (graceful?: boolean) => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onFork?: (id: string) => void;
  onRegenerate?: (userMessageId: string | number, userContent: string, agentConfigId: string, sandboxId: string) => void;
  settingsReloadKey?: number;
}

export function ChatView({
  sessionId, messages, loaded, streaming, reasoning, running, compacting,
  traceRuns, liveRunId, liveStartedAt, liveAgentName, awaiting,
  onSend, onCancel, onApprove, onReject, onFork, onRegenerate, settingsReloadKey,
}: ChatViewProps) {
  const [agentConfigId, setAgentConfigId] = useState('');
  const [sandboxId, setSandboxId] = useState('');
  const [traceOpen, setTraceOpen] = useState(false);
  const [traceActiveRun, setTraceActiveRun] = useState<string | null>(null);
  const { data: agentConfigs, reload: reloadAgents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>);
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi<SandboxConfig[]>(() => api.sandboxes.list() as Promise<SandboxConfig[]>);

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    const valid = agentConfigs.some(a => a.id === agentConfigId);
    if (!valid) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs, agentConfigId]);

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

  const handleCopyClick = useCallback((e: MouseEvent<HTMLDivElement>) => {
    const expand = (e.target as HTMLElement).closest('.btn-code-expand') as HTMLElement | null;
    if (expand) {
      expand.closest('.code-block-wrapper')?.classList.remove('code-collapsed');
      return;
    }
    const btn = (e.target as HTMLElement).closest('.btn-copy') as HTMLElement | null;
    if (!btn) return;
    const code = btn.getAttribute('data-code')
      ?.replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"');
    if (!code) return;
    navigator.clipboard.writeText(code).then(() => {
      btn.classList.add('copied');
      const svgContent = btn.innerHTML;
      btn.innerHTML = CHECK_ICON;
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svgContent; }, 1500);
    });
  }, []);

  const handleSend = useCallback((text: string) => {
    // No sessionId is fine: sending with no active session starts a new chat
    // (app-level onSend auto-creates it). Only an agent is required.
    if (!agentConfigId) return;
    onSend(text, agentConfigId, sandboxId);
  }, [agentConfigId, sandboxId, onSend]);

  const handleCancel = useCallback((graceful?: boolean) => {
    onCancel(graceful);
    toast.info(graceful ? 'Stopping after the current turn…' : 'Run cancelled');
  }, [onCancel]);

  const { turnRunMap, userRunMap, runLabels } = useMemo(() => {
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
        uMap[i] = rid;
        if (entry.content && !labels[rid]) labels[rid] = entry.content;
      } else if (entry.role === 'turn') {
        if (rid && traceRuns[rid]) {
          tMap[i] = rid;
          if (!labels[rid]) {
            let userContent: string | null = null;
            for (let j = i - 1; j >= 0; j--) {
              if (messages[j].role === 'user') {
                userContent = messages[j].content ?? null;
                if (uMap[j] === undefined) uMap[j] = rid;
                break;
              }
            }
            labels[rid] = userContent || 'Turn ' + (turnIdx + 1);
          }
        }
        turnIdx++;
      }
    }
    return { turnRunMap: tMap, userRunMap: uMap, runLabels: labels };
  }, [messages, traceRuns]);

  const openTrace = useCallback((runId: string) => {
    setTraceOpen(true);
    setTraceActiveRun(runId);
  }, []);

  // Runs that have a user message in this conversation — gates the trace
  // panel's jump-to-message control.
  const messageRunIds = useMemo(() => new Set(Object.values(userRunMap)), [userRunMap]);

  // Reverse navigation: scroll the chat to the run's user message and flash
  // it, mirroring the message → trace direction of openTrace.
  const jumpToRun = useCallback((runId: string) => {
    const el = document.querySelector(`.chat-messages [data-run-id="${CSS.escape(runId)}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    el.classList.remove('msg-jump-flash');
    // Restart the animation even when jumping to the same message twice.
    void (el as HTMLElement).offsetWidth;
    el.classList.add('msg-jump-flash');
    window.setTimeout(() => el.classList.remove('msg-jump-flash'), 1800);
  }, []);

  // Keep agent/sandbox selection in a ref so the regenerate callback stays
  // referentially stable — a new closure per render would defeat TurnBlock's memo.
  const regenConfigRef = useRef({ agentConfigId, sandboxId });
  regenConfigRef.current = { agentConfigId, sandboxId };
  const handleRegen = useCallback((messageId: string, content: string) => {
    if (!onRegenerate) return;
    onRegenerate(messageId, content, regenConfigRef.current.agentConfigId, regenConfigRef.current.sandboxId);
  }, [onRegenerate]);

  if (!sessionId) {
    return (
      <div className="chat-empty">
        <Blankslate>
          <Blankslate.Visual>
            <CommentDiscussionIcon size={24} />
          </Blankslate.Visual>
          <Blankslate.Heading>Start a conversation</Blankslate.Heading>
          <Blankslate.Description>Pick a chat from the sidebar, or create a new one to begin.</Blankslate.Description>
        </Blankslate>
      </div>
    );
  }

  const selectedSandboxLabel = sandboxConfigs?.find(s => s.id === sandboxId)?.name || 'Environment';
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
        {sandboxConfigs && sandboxConfigs.length > 0 && (
          <ActionMenu>
            <ActionMenu.Button size="small" variant="invisible" leadingVisual={ContainerIcon}>
              {selectedSandboxLabel}
            </ActionMenu.Button>
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                <ActionList.Item selected={sandboxId === ''} onSelect={() => setSandboxId('')}>None</ActionList.Item>
                {sandboxConfigs.map(s => (
                  <ActionList.Item key={s.id} selected={sandboxId === s.id} onSelect={() => setSandboxId(s.id)}>
                    {s.name}
                  </ActionList.Item>
                ))}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        )}
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

  const loading = sessionId && !loaded && messages.length === 0;
  const isEmpty = loaded && messages.length === 0;

  if (isEmpty) {
    return (
      <div className="chat-main">
        <div className="chat-content chat-content-centered">
          <Greeting />
          <MessageInput
            onSend={handleSend}
            onCancel={handleCancel}
            disabled={running || awaiting || !agentConfigId}
            running={running}
            toolbar={inputToolbar}
          />
        </div>
      </div>
    );
  }

  return (
    <div className={'chat-main' + (traceOpen ? ' trace-open' : '')}>
      <div className="chat-content">
        <div className="chat-messages-area">
        <div ref={scrollRef} className="chat-messages" onClick={handleCopyClick}>
          {loading ? null : messages.map((m, i) => {
            if (m.role === 'turn') {
              const isLive = running && i === messages.length - 1;
              let prevUserContent: string | null = null;
              let prevUserMessageId: string | undefined;
              for (let j = i - 1; j >= 0; j--) {
                if (messages[j].role === 'user') {
                  prevUserContent = messages[j].content ?? null;
                  prevUserMessageId = messages[j].messageId;
                  break;
                }
              }
              const rid = turnRunMap[i];
              const turnDuration = rid && traceRuns[rid]
                ? traceRuns[rid].find(e => e.kind === 'span' && e.duration)?.duration
                : undefined;
              return (
                <TurnBlock
                  key={'turn-' + i}
                  parts={m.parts || []}
                  streaming={isLive ? streaming : null}
                  reasoning={isLive ? reasoning : null}
                  isLive={isLive}
                  liveAgentName={isLive ? liveAgentName : null}
                  onApprove={onApprove}
                  onReject={onReject}
                  regenMessageId={prevUserMessageId ? String(prevUserMessageId) : null}
                  regenContent={prevUserContent}
                  onRegenerate={onRegenerate ? handleRegen : undefined}
                  running={running}
                  compacting={isLive ? compacting : false}
                  duration={turnDuration}
                  liveStartedAt={isLive ? liveStartedAt : undefined}
                  messageId={m.messageId}
                  onFork={onFork}
                />
              );
            }
            if (m.role === 'user') {
              const rid = userRunMap[i];
              return (
                <UserMessage
                  key={i}
                  content={m.content || ''}
                  traceRunId={rid || null}
                  onTrace={openTrace}
                />
              );
            }
            if (m.role === 'compaction') {
              return <CompactionCard key={i} content={m.content || ''} />;
            }
            return <MessageBubble key={i} role={m.role} content={m.content || ''} />;
          })}
        </div>

        {!isSticky && (
          <Button size="small" leadingVisual={ArrowDownIcon} className="scroll-to-bottom" onClick={scrollToBottom}>
            {streaming ? 'Responding…' : 'Jump to latest'}
          </Button>
        )}
        </div>

        <MessageInput
          onSend={handleSend}
          onCancel={handleCancel}
          disabled={running || awaiting || !agentConfigId}
          running={running}
          toolbar={inputToolbar}
        />
      </div>

      {traceOpen && (
        <TraceDrawer
          traceRuns={traceRuns}
          liveRunId={liveRunId}
          activeRunId={traceActiveRun}
          runLabels={runLabels}
          onClose={() => setTraceOpen(false)}
          onJumpToRun={jumpToRun}
          messageRunIds={messageRunIds}
        />
      )}
    </div>
  );
}
