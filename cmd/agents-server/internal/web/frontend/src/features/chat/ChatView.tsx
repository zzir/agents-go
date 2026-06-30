import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, type MouseEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Button, IconButton, Label, ActionMenu, ActionList } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { renderMarkdown, splitMermaidBlocks, sanitizeSVG } from '@/lib/markdown';
import { formatDuration } from '@/lib/timeline';
import { useScrollToBottom, useApi } from '@/lib/hooks';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { MessageInput } from '@/features/chat/MessageInput';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { TraceDrawer } from '@/features/chat/TracePanel';
import { ArrowDownIcon, ChevronRightIcon, RepoForkedIcon, CopyIcon, CheckIcon, SyncIcon, CommentDiscussionIcon, PulseIcon, PlusIcon, ContainerIcon, DependabotIcon } from '@primer/octicons-react';
import { toast } from '@/lib/toast';

/* ---------- types ---------- */

interface ToolCallData {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  needs_approval?: boolean;
  status?: string;
  output?: string;
}

interface TurnPart {
  type: 'text' | 'tools';
  content?: string;
  toolCalls?: ToolCallData[];
}

interface ChatMessage {
  role: string;
  content?: string;
  messageId?: string;
  parts?: TurnPart[];
}

interface TraceEventData {
  kind?: string;
  name: string;
  detail?: string;
  duration?: string;
  [key: string]: unknown;
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

  if (svg) return (
    <>
      <div className="mermaid-diagram" onClick={() => setViewerOpen(true)} dangerouslySetInnerHTML={{ __html: svg }} />
      {viewerOpen && <SvgOverlay svg={svg} onClose={() => setViewerOpen(false)} />}
    </>
  );
  if (failed) {
    return (
      <div className="code-block-wrapper">
        <pre className="hljs-code-block"><code className="hljs">{source}</code></pre>
      </div>
    );
  }
  return <pre className="mermaid-pending">{source}</pre>;
}

function TextContent({ content }: { content: string }) {
  const segments: MermaidSegment[] = useMemo(() => splitMermaidBlocks(content), [content]);
  if (segments.length === 1 && segments[0].type === 'md') {
    return <div className="turn-text markdown-body" dangerouslySetInnerHTML={{ __html: renderMarkdown(content) }} />;
  }
  return (
    <div className="turn-text markdown-body">
      {segments.map((seg, i) =>
        seg.type === 'mermaid'
          ? <MermaidBlock key={`m${i}`} source={seg.text} />
          : <div key={`t${i}`} dangerouslySetInnerHTML={{ __html: renderMarkdown(seg.text) }} />
      )}
    </div>
  );
}

interface ProcessGroupProps {
  toolCalls: ToolCallData[];
  onApprove?: (id: string) => void;
  onReject?: (id: string) => void;
}

function ProcessGroup({ toolCalls, onApprove, onReject }: ProcessGroupProps) {
  const [expanded, setExpanded] = useState(false);
  const count = toolCalls.length;
  if (count === 0) return null;

  const pendingCount = toolCalls.filter(tc => tc.needs_approval && !tc.status).length;
  const completedCount = toolCalls.filter(tc => tc.status === 'completed' || tc.output).length;
  const isRunning = completedCount < count && pendingCount === 0;

  const shouldShow = expanded || pendingCount > 0;

  return (
    <div className="process-group">
      <div
        className={'process-group-toggle' + (shouldShow ? ' expanded' : '')}
        onClick={() => setExpanded(!expanded)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>{count + ' tool call' + (count > 1 ? 's' : '')}</span>
        {pendingCount > 0 && <Label variant="accent" className="process-status">{pendingCount + ' pending'}</Label>}
        {isRunning && <Label variant="secondary" className="process-status">running...</Label>}
      </div>
      {shouldShow && (
        <div className="process-group-body">
          {toolCalls.map(tc => (
            <ToolCallCard
              key={tc.tool_call_id}
              toolCall={tc}
              onApprove={onApprove}
              onReject={onReject}
            />
          ))}
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
  isLive: boolean;
  onApprove?: (id: string) => void;
  onReject?: (id: string) => void;
  turnText: string;
  onRegenerate: (() => void) | null;
  running: boolean;
  duration?: string;
  liveStartedAt?: number | null;
  messageId?: string | number;
  onFork?: (id: string) => void;
}

function TurnBlock({ parts, streaming, isLive, onApprove, onReject, turnText, onRegenerate, running, duration, liveStartedAt, messageId, onFork }: TurnBlockProps) {
  const isEmpty = parts.length === 0 && !streaming;
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!turnText) return;
    navigator.clipboard.writeText(turnText).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [turnText]);

  return (
    <div className="message message-turn">
      {parts.map((part, i) => {
        if (part.type === 'text') {
          return <TextContent key={'p-' + i} content={part.content!} />;
        }
        if (part.type === 'tools') {
          return <ProcessGroup key={'p-' + i} toolCalls={part.toolCalls!} onApprove={onApprove} onReject={onReject} />;
        }
        return null;
      })}
      {streaming && (
        <div
          className="turn-text markdown-body streaming"
          dangerouslySetInnerHTML={{ __html: renderMarkdown(streaming + '▋') }}
        />
      )}
      {isLive && isEmpty && (
        <div className="thinking-indicator">
          <div className="thinking-dots">
            <span /><span /><span />
          </div>
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
          {!running && onRegenerate && (
            <IconButton
              icon={SyncIcon}
              variant="invisible"
              size="small"
              aria-label="Regenerate"
              onClick={onRegenerate}
            />
          )}
          {duration && <span className="turn-duration">{duration}</span>}
        </div>
      )}
    </div>
  );
}

function CompactionCard({ content }: { content: string }) {
  const [expanded, setExpanded] = useState(false);
  const summaryText = content.replace(/^\[Conversation Summary\]\s*/, '');
  const summaryHtml = useMemo(() => renderMarkdown(summaryText), [summaryText]);

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
}

interface UserMessageProps {
  content: string;
  running: boolean;
  onTrace?: () => void;
  hasTrace: boolean;
}

function UserMessage({ content, onTrace, hasTrace }: UserMessageProps) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!content) return;
    navigator.clipboard.writeText(content).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [content]);

  return (
    <div className="message message-user message-forkable">
      <div className="message-body">{content}</div>
      <div className="message-user-actions">
        {hasTrace && onTrace && (
          <IconButton
            icon={PulseIcon}
            variant="invisible"
            size="small"
            aria-label="Trace"
            onClick={onTrace}
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
}

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
  running: boolean;
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  lastError?: string;
  onSend: (text: string, agentConfigId: string, sandboxId: string) => void;
  onCancel: () => void;
  onApprove?: (id: string) => void;
  onReject?: (id: string) => void;
  onFork?: (id: string) => void;
  onRegenerate?: (userMessageId: string | number, userContent: string, agentConfigId: string, sandboxId: string) => void;
  settingsReloadKey?: number;
}

export function ChatView({
  sessionId, messages, loaded, streaming, running,
  traceRuns, liveRunId, liveStartedAt, lastError,
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

  const { ref: scrollRef, isSticky, scrollToBottom } = useScrollToBottom(messages.length + (streaming ? 1 : 0), sessionId);

  useEffect(() => {
    if (lastError) toast.error(lastError);
  }, [lastError]);

  const handleCopyClick = useCallback((e: MouseEvent<HTMLDivElement>) => {
    const btn = (e.target as HTMLElement).closest('.btn-copy') as HTMLElement | null;
    if (!btn) return;
    const code = btn.getAttribute('data-code')
      ?.replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"');
    if (!code) return;
    navigator.clipboard.writeText(code).then(() => {
      btn.classList.add('copied');
      const svgContent = btn.innerHTML;
      btn.innerHTML = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>';
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svgContent; }, 1500);
    });
  }, []);

  const handleSend = useCallback((text: string) => {
    if (!sessionId || !agentConfigId) return;
    onSend(text, agentConfigId, sandboxId);
  }, [sessionId, agentConfigId, sandboxId, onSend]);

  const handleCancel = useCallback(() => {
    onCancel();
    toast.info('Run cancelled');
  }, [onCancel]);

  const { turnRunMap, userRunMap, runLabels } = useMemo(() => {
    const tMap: Record<number, string> = {};
    const uMap: Record<number, string> = {};
    const labels: Record<string, string> = {};
    let turnIdx = 0;
    for (let i = 0; i < messages.length; i++) {
      const entry = messages[i] as any;
      if (entry.role === 'turn') {
        const rid = entry.runId;
        if (rid && traceRuns[rid]) {
          tMap[i] = rid;
          let userContent: string | null = null;
          for (let j = i - 1; j >= 0; j--) {
            if (messages[j].role === 'user') {
              userContent = messages[j].content ?? null;
              uMap[j] = rid;
              break;
            }
          }
          labels[rid] = userContent || 'Turn ' + (turnIdx + 1);
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
            disabled={running || !agentConfigId}
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
              const turnText = (m.parts || []).filter(p => p.type === 'text').map(p => p.content || '').join('\n\n');
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
              const regenHandler = prevUserContent && prevUserMessageId && onRegenerate
                ? () => onRegenerate(prevUserMessageId!, prevUserContent!, agentConfigId, sandboxId)
                : null;
              return (
                <TurnBlock
                  key={'turn-' + i}
                  parts={m.parts || []}
                  streaming={isLive ? streaming : null}
                  isLive={isLive}
                  onApprove={onApprove}
                  onReject={onReject}
                  turnText={turnText}
                  onRegenerate={regenHandler}
                  running={running}
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
                  running={running}
                  hasTrace={!!rid}
                  onTrace={rid ? () => openTrace(rid) : undefined}
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
          disabled={running || !agentConfigId}
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
        />
      )}
    </div>
  );
}
