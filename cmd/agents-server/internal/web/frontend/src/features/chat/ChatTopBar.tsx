import { ActionList, ActionMenu, IconButton } from '@primer/react';
import { BrowserIcon, DownloadIcon, FileDirectoryIcon, KeyAsteriskIcon, KebabHorizontalIcon, MeterIcon, PlayIcon, PulseIcon, SquareFillIcon, StackIcon, SyncIcon, TerminalIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import type { InspectorPanel } from '@/features/chat/ChatView';
import { useChatSession } from '@/features/chat/ChatSessionContext';

interface ChatTopBarProps {
  sessionName: string;
  panel: InspectorPanel;
  onPanelChange: (panel: InspectorPanel) => void;
  /* The terminal panel opens from the project menu, not from a button of its
     own: what it opens is this project's terminal, and the three buttons on
     the right are inspector lenses — it never belonged among them. The cost
     is that an unbound session has no way in, which is the trade taken: a
     session binds on its first message. */
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

/* What the bound project offers: its terminal, the environment its container
   is created with, starting and stopping the compute, and the way back from a
   container someone broke. */
export interface ProjectMenu {
  busy: boolean;
  /* absent | stopped | running, or '' while unknown. */
  state: string;
  /* False on a backend where the sandbox IS the storage, and replacing it
     would take the working tree with it. */
  rebuildable: boolean;
  onEnv: () => void;
  /* The ports this project publishes — what Preview can open. Empty offers
     the settings dialog instead of a link that cannot work. */
  ports: number[];
  onPreview: (port: number) => void;
  onStart: () => void;
  onStop: () => void;
  onExport: () => void;
  onRebuild: () => void;
  /* Re-read the compute state as the menu opens: a run that started the
     sandbox did not tell this component. */
  onOpen: () => void;
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
          <ActionMenu onOpenChange={open => { if (open) projectMenu.onOpen(); }}>
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
                <ActionList.Item disabled={!terminalEnabled} onSelect={() => onTerminalOpen?.()}>
                  <ActionList.LeadingVisual><TerminalIcon /></ActionList.LeadingVisual>
                  Terminal panel
                </ActionList.Item>
                <ActionList.Item onSelect={projectMenu.onEnv}>
                  <ActionList.LeadingVisual><KeyAsteriskIcon /></ActionList.LeadingVisual>
                  Settings…
                </ActionList.Item>
                {projectMenu.ports.length === 0 ? (
                  // Nothing to open: the item leads where a port is declared,
                  // rather than asking for one the container never published.
                  <ActionList.Item onSelect={projectMenu.onEnv}>
                    <ActionList.LeadingVisual><BrowserIcon /></ActionList.LeadingVisual>
                    Publish a port to preview…
                  </ActionList.Item>
                ) : projectMenu.ports.map(port => (
                  <ActionList.Item key={port} onSelect={() => projectMenu.onPreview(port)}>
                    <ActionList.LeadingVisual><BrowserIcon /></ActionList.LeadingVisual>
                    Preview port {port}
                  </ActionList.Item>
                ))}
                <ActionList.Item onSelect={projectMenu.onExport}>
                  <ActionList.LeadingVisual><DownloadIcon /></ActionList.LeadingVisual>
                  Export as tar…
                </ActionList.Item>
                <ActionList.Divider />
                {projectMenu.state === 'running' ? (
                  <ActionList.Item onSelect={projectMenu.onStop}>
                    <ActionList.LeadingVisual><SquareFillIcon /></ActionList.LeadingVisual>
                    Stop sandbox
                  </ActionList.Item>
                ) : (
                  <ActionList.Item onSelect={projectMenu.onStart}>
                    <ActionList.LeadingVisual><PlayIcon /></ActionList.LeadingVisual>
                    Start sandbox
                  </ActionList.Item>
                )}
                {projectMenu.rebuildable && (
                  <ActionList.Item variant="danger" onSelect={projectMenu.onRebuild}>
                    <ActionList.LeadingVisual><SyncIcon /></ActionList.LeadingVisual>
                    Rebuild container
                  </ActionList.Item>
                )}
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
      </div>
    </div>
  );
}
