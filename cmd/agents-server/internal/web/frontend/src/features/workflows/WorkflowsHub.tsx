import { useEffect, useState } from 'react';
import { SegmentedControl } from '@primer/react';
import { HistoryIcon, WorkflowIcon, ZapIcon } from '@primer/octicons-react';
import { WorkflowPanel } from '@/features/workflows/WorkflowPanel';
import { TriggersView } from '@/features/workflows/TriggersView';
import { RunsView } from '@/features/workflows/RunsView';
import './hub.css';

// The hub's three views: what a workflow IS, what fires it on its own, and
// every execution across conversations. One place, because a workflow is
// authored once, then watched — and the watching is what a settings dialog
// cannot host.
export type HubTab = 'definitions' | 'triggers' | 'runs';

export const HUB_TABS: HubTab[] = ['definitions', 'triggers', 'runs'];

interface WorkflowsHubProps {
  tab: HubTab;
  onTabChange: (tab: HubTab) => void;
  // The conversation the person was in: the default target of a Run… and a
  // new trigger.
  sessionId: string | null;
  // Moves whenever any execution does, so the Runs view follows live work.
  tasksSig: string;
  onOpenRun: (sessionId: string, taskId: string) => void;
}

// WorkflowsHub is the middle column when the sidebar's Workflows entry is
// selected. Its header sits on the same 48px line as the chat top bar.
export function WorkflowsHub({ tab, onTabChange, sessionId, tasksSig, onOpenRun }: WorkflowsHubProps) {
  // Keep-alive: a view is mounted on first visit and then kept (hidden), so
  // switching back is instant — its data, scroll and pagination intact —
  // instead of re-mounting from an empty state (the flash). RunsView stays
  // fresh off tasksSig even while hidden; its live ticker stands down until
  // it is the shown view again.
  const [visited, setVisited] = useState<Set<HubTab>>(() => new Set([tab]));
  useEffect(() => {
    setVisited(prev => (prev.has(tab) ? prev : new Set(prev).add(tab)));
  }, [tab]);

  return (
    <div className="hub">
      <div className="chat-topbar hub-topbar">
        <div className="chat-topbar-info">
          <div className="chat-topbar-title" id="hub-runs-title">Workflows</div>
        </div>
        {/* Primer's own sizing: each segment as wide as its label. Stretching
            them equal makes the hover fill of a short label read as a block. */}
        <SegmentedControl aria-label="Workflows view" size="small" onChange={i => onTabChange(HUB_TABS[i] || 'definitions')}>
          <SegmentedControl.Button selected={tab === 'definitions'} leadingIcon={WorkflowIcon}>Definitions</SegmentedControl.Button>
          <SegmentedControl.Button selected={tab === 'triggers'} leadingIcon={ZapIcon}>Triggers</SegmentedControl.Button>
          <SegmentedControl.Button selected={tab === 'runs'} leadingIcon={HistoryIcon}>Runs</SegmentedControl.Button>
        </SegmentedControl>
        <div className="chat-topbar-info" aria-hidden="true" />
      </div>
      <div className="hub-body">
        {visited.has('definitions') && (
          <div className="hub-view" hidden={tab !== 'definitions'}>
            <div className="hub-content"><WorkflowPanel sessionId={sessionId} /></div>
          </div>
        )}
        {visited.has('triggers') && (
          <div className="hub-view" hidden={tab !== 'triggers'}>
            <div className="hub-content"><TriggersView sessionId={sessionId} /></div>
          </div>
        )}
        {visited.has('runs') && (
          <div className="hub-view" hidden={tab !== 'runs'}>
            <div className="hub-content"><RunsView version={tasksSig} onOpenRun={onOpenRun} active={tab === 'runs'} /></div>
          </div>
        )}
      </div>
    </div>
  );
}
