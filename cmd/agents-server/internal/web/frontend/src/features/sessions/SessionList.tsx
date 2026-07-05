import './sessions.css';
import { useState, useEffect, useRef, type ReactElement, type RefObject } from 'react';
import { ActionList, ActionMenu } from '@primer/react';
import { ClockIcon, KebabHorizontalIcon, PinIcon, PinSlashIcon, PlusIcon, RepoForkedIcon, TrashIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';

interface Session {
  id: string;
  name: string;
  pinned: boolean;
  created_at: string;
  updated_at: string;
}

interface SessionItemProps {
  s: Session;
  activeId: string | null;
  isRunning: boolean;
  isAwaiting: boolean;
  onSelect: (id: string | null) => void;
  onPin: (id: string, pinned: boolean) => void;
  onFork: (id: string) => void;
  onDelete: (id: string) => void;
}

interface SessionListProps {
  activeId: string | null;
  onSelect: (id: string | null) => void;
  onDelete?: (id: string) => void;
  onCreated?: () => void;
  reloadKey: unknown;
  runningSessions?: Set<string>;
  awaitingSessions?: Set<string>;
}

function SessionItem({ s, activeId, isRunning, isAwaiting, onSelect, onPin, onFork, onDelete }: SessionItemProps): ReactElement {
  const [menuOpen, setMenuOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement>(null);
  const isActive = s.id === activeId;
  return (
    <ActionList.Item
      active={isActive || isRunning || isAwaiting}
      onSelect={() => onSelect(s.id)}
    >
      {/* Awaiting approval (red) takes precedence over running (orange): a
          paused run is still "running" live, but the red bar is the signal that
          needs the user's attention, so the markers are mutually exclusive. */}
      {isAwaiting && <span className="session-awaiting" hidden />}
      {isRunning && !isAwaiting && <span className="session-running" hidden />}
      {isActive && <span className="session-selected" hidden />}
      {s.name}
      {/* TrailingAction renders as a sibling of the item's button inside the
          <li>, unlike TrailingVisual which would nest a button in a button. */}
      <ActionList.TrailingAction
        ref={anchorRef}
        className="session-kebab"
        icon={KebabHorizontalIcon}
        label="Session actions"
        onClick={() => setMenuOpen(o => !o)}
      />
      <ActionMenu open={menuOpen} onOpenChange={setMenuOpen} anchorRef={anchorRef as RefObject<HTMLElement>}>
        <ActionMenu.Overlay>
          <ActionList>
            <ActionList.Item onSelect={() => onPin(s.id, !s.pinned)}>
              <ActionList.LeadingVisual>
                {s.pinned ? <PinSlashIcon size={16} /> : <PinIcon size={16} />}
              </ActionList.LeadingVisual>
              {s.pinned ? 'Unpin' : 'Pin'}
            </ActionList.Item>
            <ActionList.Item onSelect={() => onFork(s.id)}>
              <ActionList.LeadingVisual><RepoForkedIcon size={16} /></ActionList.LeadingVisual>
              Fork
            </ActionList.Item>
            <ActionList.Divider />
            <ActionList.Item variant="danger" onSelect={() => onDelete(s.id)}>
              <ActionList.LeadingVisual><TrashIcon size={16} /></ActionList.LeadingVisual>
              Delete
            </ActionList.Item>
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
    </ActionList.Item>
  );
}

export function SessionList({ activeId, onSelect, onDelete: onDeleteNotify, onCreated, reloadKey, runningSessions, awaitingSessions }: SessionListProps): ReactElement {
  const { data: sessions, reload } = useApi(() => api.sessions.list() as Promise<Session[]>);

  useEffect(() => {
    if (reloadKey) reload();
  }, [reloadKey, reload]);
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    setCreating(true);
    try {
      const sess = await api.sessions.create('New Chat') as Session;
      await reload();
      onSelect(sess.id);
      if (onCreated) onCreated();
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async (id: string) => {
    await api.sessions.delete(id);
    reload();
    if (onDeleteNotify) onDeleteNotify(id);
    if (activeId === id) onSelect(null);
  };

  const handleFork = async (id: string) => {
    const forked = await api.sessions.fork(id) as Session;
    await reload();
    onSelect(forked.id);
  };

  const handlePin = async (id: string, pinned: boolean) => {
    await api.sessions.pin(id, pinned);
    reload();
  };

  const pinned = sessions ? sessions.filter(s => s.pinned) : [];
  const recents = sessions ? sessions.filter(s => !s.pinned) : [];
  const loaded = sessions !== null;

  const renderItem = (s: Session) => (
    <SessionItem
      key={s.id}
      s={s}
      activeId={activeId}
      isRunning={!!(runningSessions && runningSessions.has(s.id))}
      isAwaiting={!!(awaitingSessions && awaitingSessions.has(s.id))}
      onSelect={onSelect}
      onPin={handlePin}
      onFork={handleFork}
      onDelete={handleDelete}
    />
  );

  return (
    <>
      <div className="sidebar-actions">
        <ActionList>
          <ActionList.Item onSelect={handleCreate} disabled={creating}>
            <ActionList.LeadingVisual><PlusIcon size={16} /></ActionList.LeadingVisual>
            New Chat
          </ActionList.Item>
          <ActionList.Item disabled>
            <ActionList.LeadingVisual><ClockIcon size={16} /></ActionList.LeadingVisual>
            Automation
          </ActionList.Item>
        </ActionList>
      </div>
      <div className="sidebar-scroll">
        {loaded && (
          <ActionList>
            {pinned.length > 0 && (
              <ActionList.Group>
                {/* Primer requires an explicit heading level on list-role
                    ActionLists; omitting `as` throws and unmounts the app. */}
                <ActionList.GroupHeading as="h3">Pinned</ActionList.GroupHeading>
                {pinned.map(renderItem)}
              </ActionList.Group>
            )}
            {pinned.length > 0 ? (
              <ActionList.Group>
                <ActionList.GroupHeading as="h3">Recents</ActionList.GroupHeading>
                {recents.length > 0
                  ? recents.map(renderItem)
                  : <div className="blankslate">No conversations yet</div>
                }
              </ActionList.Group>
            ) : (
              recents.length > 0
                ? recents.map(renderItem)
                : <div className="blankslate">No conversations yet</div>
            )}
          </ActionList>
        )}
      </div>
    </>
  );
}
