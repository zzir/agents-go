import { IconButton } from '@primer/react';
import { FileDirectoryIcon, MeterIcon, PulseIcon, StackIcon, TerminalIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import type { InspectorPanel } from '@/features/chat/ChatView';
import { projectBase } from '@/lib/binding';

interface ChatTopBarProps {
  sessionName: string;
  sessionId: string | null;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  // Tasks and workflow executions both — the panel holds either, so a session
  // with only a workflow must not find the button greyed out.
  backgroundCount: number;
  terminalEnabled: boolean;
  onTerminalOpen?: () => void;
  /* The session's sandbox binding, rendered as a quiet read-only label beside
     the title once the first sandbox-carrying run has fixed it. The binding is
     permanent — switching projects means starting a new session (the
     composer's Project picker), so there is nothing to edit here. Shows only
     the workdir basename; the full path and sandbox name live in the hover
     title. */
  binding?: { title: string; workDir: string } | null;
}

export function ChatTopBar({
  sessionName,
  sessionId,
  panel,
  onPanelChange,
  backgroundCount,
  terminalEnabled,
  onTerminalOpen,
  binding,
}: ChatTopBarProps): ReactElement {
  return (
    <div className="chat-topbar">
      <div className="chat-topbar-info">
        <div className="chat-topbar-title">{sessionName}</div>
        {binding && (
          // Quiet metadata next to the title: just the directory's basename —
          // the name a person knows the project by — in muted text.
          <span className="chat-topbar-binding" title={binding.title}>
            <FileDirectoryIcon size={12} />
            <span className="chat-topbar-binding-path">
              {projectBase(binding.workDir)}
            </span>
          </span>
        )}
      </div>
      <div className="chat-topbar-actions">
        <IconButton
          icon={StackIcon}
          variant="invisible"
          size="small"
          aria-label="Tasks"
          onClick={() => onPanelChange(panel?.kind === 'tasks' ? null : { kind: 'tasks' })}
          disabled={!sessionId || backgroundCount === 0}
        />
        <IconButton
          icon={PulseIcon}
          variant="invisible"
          size="small"
          aria-label="Traces"
          // Not gated on having spans in memory: they load lazily when this
          // panel first opens, so a count gate would lock the door that loads
          // them.
          disabled={!sessionId}
          onClick={() => onPanelChange(panel?.kind === 'trace' ? null : { kind: 'trace' })}
        />
        <IconButton
          icon={MeterIcon}
          variant="invisible"
          size="small"
          aria-label="Context"
          disabled={!sessionId}
          onClick={() => onPanelChange(panel?.kind === 'context' ? null : { kind: 'context' })}
        />
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
