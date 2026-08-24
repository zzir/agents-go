import { IconButton } from '@primer/react';
import { FileDirectoryIcon, MeterIcon, PulseIcon, StackIcon, TerminalIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import type { InspectorPanel } from '@/features/chat/ChatView';
import { useChatSession } from '@/features/chat/ChatSessionContext';
import { useIsAdmin } from '@/lib/me';

interface ChatTopBarProps {
  sessionName: string;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  terminalEnabled: boolean;
  onTerminalOpen?: () => void;
  /* The session's sandbox binding, rendered as a quiet read-only label beside
     the title once the first sandbox-carrying run has fixed it. The binding is
     permanent — switching projects means starting a new session (the
     composer's Project picker), so there is nothing to edit here. Shows only
     the project name; the sandbox name lives in the hover title. */
  binding?: { title: string; projectName: string } | null;
}

export function ChatTopBar({
  sessionName,
  panel,
  onPanelChange,
  terminalEnabled,
  onTerminalOpen,
  binding,
}: ChatTopBarProps): ReactElement {
  const { sessionId } = useChatSession();
  // /ws/terminal is admin-only server-side; a member gets no button rather
  // than one that fails. Unknown yet (loading) keeps it, disabled as usual.
  const isAdmin = useIsAdmin();
  return (
    <div className="chat-topbar">
      <div className="chat-topbar-info">
        <div className="chat-topbar-title">{sessionName}</div>
        {binding && (
          // Quiet metadata next to the title: just the project's name — the
          // name a person knows it by — in muted text.
          <span className="chat-topbar-binding" title={binding.title}>
            <FileDirectoryIcon size={12} />
            <span className="chat-topbar-binding-path">
              {binding.projectName}
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
          // Same gate as the other lenses: an empty session opens to its
          // empty state, not a greyed-out button.
          disabled={!sessionId}
          onClick={() => onPanelChange(panel?.kind === 'tasks' ? null : { kind: 'tasks' })}
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
        {isAdmin !== false && (
          <IconButton
            icon={TerminalIcon}
            variant="invisible"
            size="small"
            aria-label="Terminal"
            disabled={!terminalEnabled}
            onClick={onTerminalOpen}
          />
        )}
      </div>
    </div>
  );
}
