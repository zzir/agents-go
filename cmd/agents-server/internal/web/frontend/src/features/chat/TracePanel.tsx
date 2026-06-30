import './trace.css';
import { useState, useEffect, useMemo, useRef } from 'react';
import { IconButton, CounterLabel } from '@primer/react';
import {
  XIcon, PulseIcon, ChevronRightIcon,
  PlayIcon, SquareCircleIcon, ArrowRightIcon, ArrowLeftIcon,
  ToolsIcon, CheckIcon, ArrowSwitchIcon, DotFillIcon, DiamondIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';

interface TraceEventData {
  kind?: string;
  name: string;
  detail?: string;
  agent?: string;
  tool?: string;
  from?: string;
  to?: string;
  type?: string;
  duration?: string;
}

interface HookMeta {
  label: string;
  color: string;
  icon: Icon;
}

const HOOK_META: Record<string, HookMeta> = {
  agent_start:  { label: 'Agent Start',  color: 'var(--fgColor-open)',    icon: PlayIcon },
  agent_end:    { label: 'Agent End',    color: 'var(--fgColor-closed)',   icon: SquareCircleIcon },
  llm_start:    { label: 'LLM Call',     color: 'var(--fgColor-accent)',   icon: ArrowRightIcon },
  llm_end:      { label: 'LLM Response', color: 'var(--fgColor-accent)',   icon: ArrowLeftIcon },
  tool_start:   { label: 'Tool Call',    color: 'var(--fgColor-done)',     icon: ToolsIcon },
  tool_end:     { label: 'Tool Result',  color: 'var(--fgColor-done)',     icon: CheckIcon },
  handoff:      { label: 'Handoff',      color: 'var(--fgColor-severe)',   icon: ArrowSwitchIcon },
  compaction:   { label: 'Compaction',   color: 'var(--fgColor-attention)', icon: DiamondIcon },
};

interface Tokens {
  input: number;
  output: number;
}

function parseTokens(detail: string | undefined): Tokens | null {
  if (!detail) return null;
  const m = detail.match(/input=(\d+)\s+output=(\d+)/);
  return m ? { input: parseInt(m[1]), output: parseInt(m[2]) } : null;
}

function TraceEvent({ ev }: { ev: TraceEventData }) {
  if (ev.kind === 'hook') {
    const meta = HOOK_META[ev.name] || { label: ev.name, color: 'var(--fgColor-muted)', icon: DotFillIcon };
    const tokens = ev.name === 'llm_end' ? parseTokens(ev.detail) : null;
    const HookIcon = meta.icon;

    return (
      <div className="trace-ev">
        <span className="trace-ev-icon" style={{ color: meta.color }}><HookIcon size={12} /></span>
        <div className="trace-ev-body">
          <span className="trace-ev-label">{meta.label}</span>
          {ev.agent && <span className="trace-ev-tag">{ev.agent}</span>}
          {ev.tool && <span className="trace-ev-tag trace-ev-tag-tool">{ev.tool}</span>}
          {ev.from && ev.to && <span className="trace-ev-tag">{ev.from + ' → ' + ev.to}</span>}
          {tokens && (
            <span className="trace-ev-tokens">
              <span>{'↑' + tokens.input}</span>
              <span>{'↓' + tokens.output}</span>
            </span>
          )}
          {ev.name === 'agent_end' && ev.detail && !tokens && (
            <span className="trace-ev-detail" title={ev.detail}>
              {ev.detail.length > 50 ? ev.detail.slice(0, 50) + '…' : ev.detail}
            </span>
          )}
        </div>
      </div>
    );
  }

  return (
    <div className="trace-ev">
      <span className="trace-ev-icon trace-ev-icon-span"><DiamondIcon size={12} /></span>
      <div className="trace-ev-body">
        <span className="trace-ev-label">{ev.name}</span>
        {ev.type && <span className="trace-ev-tag trace-ev-tag-span">{ev.type}</span>}
        {ev.duration && <span className="trace-ev-duration">{ev.duration}</span>}
      </div>
    </div>
  );
}

interface TraceRunProps {
  runId: string;
  events: TraceEventData[];
  label: string;
  isLive: boolean;
  isExpanded: boolean;
  onToggle: () => void;
}

function TraceRun({ events, label, isLive, isExpanded, onToggle }: TraceRunProps) {
  const ref = useRef<HTMLDivElement>(null);
  const tokens = useMemo(() => {
    let inp = 0, out = 0;
    for (const ev of events) {
      if (ev.name === 'llm_end') {
        const t = parseTokens(ev.detail);
        if (t) { inp += t.input; out += t.output; }
      }
    }
    return inp > 0 ? { input: inp, output: out } : null;
  }, [events]);

  useEffect(() => {
    if (isExpanded && ref.current) {
      ref.current.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
  }, [isExpanded]);

  return (
    <div ref={ref} className={'trace-run' + (isExpanded ? ' expanded' : '')}>
      <button className="trace-run-header" onClick={onToggle}>
        <ChevronRightIcon size={14} className="trace-run-chevron" />
        <span className="trace-run-label">{label}</span>
        {isLive && <span className="trace-tab-live" />}
        {tokens && (
          <span className="trace-run-tokens">
            {(tokens.input + tokens.output).toLocaleString() + ' tok'}
          </span>
        )}
        <CounterLabel>{events.length}</CounterLabel>
      </button>
      {isExpanded && (
        <div className="trace-run-body">
          {events.length === 0 && <div className="trace-empty">No trace events.</div>}
          {events.map((ev, i) => <TraceEvent key={i} ev={ev} />)}
        </div>
      )}
    </div>
  );
}

interface TraceDrawerProps {
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  activeRunId: string | null;
  runLabels: Record<string, string>;
  onClose: () => void;
}

export function TraceDrawer({ traceRuns, liveRunId, activeRunId, runLabels, onClose }: TraceDrawerProps) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const runIds = useMemo(() => Object.keys(traceRuns), [traceRuns]);

  useEffect(() => {
    if (activeRunId && traceRuns[activeRunId]) {
      setExpanded(prev => ({ ...prev, [activeRunId]: true }));
    }
  }, [activeRunId, traceRuns]);

  const toggle = (rid: string) => setExpanded(prev => ({ ...prev, [rid]: !prev[rid] }));

  return (
    <div className="trace-drawer">
      <div className="trace-drawer-header">
        <div className="trace-drawer-title">
          <PulseIcon size={16} />
          <span>Traces</span>
          <CounterLabel>{runIds.length}</CounterLabel>
        </div>
        <IconButton icon={XIcon} variant="invisible" aria-label="Close" onClick={onClose} />
      </div>

      <div className="trace-drawer-body">
        {runIds.length === 0 && (
          <div className="trace-empty">No traces yet.</div>
        )}
        {runIds.map(rid => (
          <TraceRun
            key={rid}
            runId={rid}
            events={traceRuns[rid] || []}
            label={(runLabels && runLabels[rid]) || rid.slice(0, 8)}
            isLive={rid === liveRunId}
            isExpanded={!!expanded[rid]}
            onToggle={() => toggle(rid)}
          />
        ))}
      </div>
    </div>
  );
}
