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
  // Definitions are shared configuration: admin-written. A member still runs
  // one and sets triggers on it (their own acts, into their own sessions).
  // null while the role is still unknown: neither the editor nor the
  // member's notice shows.
  canEdit: boolean | null;
}

// WorkflowsHub is the middle column when the sidebar's Workflows entry is
// selected. Its header sits on the same 48px line as the chat top bar.
export function WorkflowsHub({ tab, onTabChange, sessionId, tasksSig, onOpenRun, canEdit }: WorkflowsHubProps) {
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
        <div className="hub-content">
          {tab === 'definitions' && <WorkflowPanel sessionId={sessionId} canEdit={canEdit} />}
          {tab === 'triggers' && <TriggersView sessionId={sessionId} />}
          {tab === 'runs' && <RunsView version={tasksSig} onOpenRun={onOpenRun} />}
        </div>
      </div>
    </div>
  );
}
