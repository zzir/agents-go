import { ActionList, ActionMenu, IconButton } from '@primer/react';
import { FileDirectoryIcon, KebabHorizontalIcon, MeterIcon, PulseIcon, StackIcon, TerminalIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import type { InspectorPanel } from '@/features/chat/ChatView';
import { useChatSession } from '@/features/chat/ChatSessionContext';

interface ChatTopBarProps {
  sessionName: string;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  terminalEnabled: boolean;
  onTerminalOpen?: () => void;
  /* The session's sandbox binding, rendered as a quiet read-only label beside
     the title once the first sandbox-carrying run has fixed it. WHICH tree is
     permanent — switching projects means starting a new session (the
     composer's Project picker) — but what its container is configured with is
     not, which is what the menu beside it edits. Shows only the project name;
     the sandbox name lives in the hover title. */
  binding?: { title: string; projectName: string } | null;
  /* The bound project's own actions. Absent while unbound: there is no
     container to act on yet. */
  projectMenu?: ProjectMenu | null;
}

/* What the bound project offers: editing the environment its container is
   created with, and the two container calls. */
export interface ProjectMenu {
  label: string;
  busy: boolean;
  onEnv: () => void;
  onPrepare: () => void;
  onRebuild: () => void;
}

export function ChatTopBar({
  sessionName,
  panel,
  onPanelChange,
  terminalEnabled,
  onTerminalOpen,
  binding,
  projectMenu,
}: ChatTopBarProps): ReactElement {
  const { sessionId } = useChatSession();
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
        {binding && projectMenu && (
          <ActionMenu>
            <ActionMenu.Anchor>
              <IconButton
                icon={KebabHorizontalIcon}
                variant="invisible"
                size="small"
                aria-label={`Actions for ${binding.projectName}`}
                disabled={projectMenu.busy}
              />
            </ActionMenu.Anchor>
            <ActionMenu.Overlay>
              <ActionList>
                <ActionList.Group>
                  <ActionList.GroupHeading as="h3">{projectMenu.label}</ActionList.GroupHeading>
                  <ActionList.Item onSelect={projectMenu.onEnv}>
                    Environment variables…
                  </ActionList.Item>
                  <ActionList.Item onSelect={projectMenu.onPrepare}>
                    Prepare container
                    <ActionList.Description variant="block">
                      Create it now instead of on the next run
                    </ActionList.Description>
                  </ActionList.Item>
                  <ActionList.Item variant="danger" onSelect={projectMenu.onRebuild}>
                    Rebuild container…
                    <ActionList.Description variant="block">
                      Discard it and start from the image again
                    </ActionList.Description>
                  </ActionList.Item>
                </ActionList.Group>
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
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
