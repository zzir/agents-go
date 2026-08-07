import { IconButton } from '@primer/react';
import { DiffIcon, PulseIcon, StackIcon, TerminalIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import type { InspectorPanel } from '@/features/chat/ChatView';
import type { TaskState } from '@/lib/useAgentSocket';

interface ChatTopBarProps {
  sessionName: string;
  sessionId: string | null;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  tasks?: Record<string, TaskState>;
  traceCount: number;
  terminalEnabled: boolean;
  onTerminalOpen?: () => void;
}

export function ChatTopBar({
  sessionName,
  sessionId,
  panel,
  onPanelChange,
  tasks,
  traceCount,
  terminalEnabled,
  onTerminalOpen,
}: ChatTopBarProps): ReactElement {
  const taskList = tasks ? Object.values(tasks) : [];
  return (
    <div className="chat-topbar">
      <div className="chat-topbar-title">{sessionName}</div>
      <div className="chat-topbar-actions">
        <IconButton
          icon={StackIcon}
          variant="invisible"
          size="small"
          aria-label="Tasks"
          onClick={() => onPanelChange(panel?.kind === 'tasks' ? null : { kind: 'tasks' })}
          disabled={!sessionId || taskList.length === 0}
        />
        <IconButton
          icon={PulseIcon}
          variant="invisible"
          size="small"
          aria-label="Traces"
          disabled={!sessionId || traceCount === 0}
          onClick={() => onPanelChange(panel?.kind === 'trace' ? null : { kind: 'trace' })}
        />
        <IconButton icon={DiffIcon} variant="invisible" size="small" aria-label="Diff" disabled />
        <IconButton
          icon={TerminalIcon}
          variant="invisible"
          size="small"
          aria-label="Terminal"
          disabled={!terminalEnabled}
          onClick={onTerminalOpen}
        />
      </div>
    </div>
  );
}
