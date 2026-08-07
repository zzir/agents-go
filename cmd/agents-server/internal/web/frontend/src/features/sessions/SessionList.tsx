import './sessions.css';
import { useState, useEffect, useRef, type ReactElement, type RefObject, type SyntheticEvent } from 'react';
import { ActionList, ActionMenu, IconButton, TextInput } from '@primer/react';
import { KebabHorizontalIcon, PinIcon, PinSlashIcon, PlusIcon, RepoForkedIcon, SearchIcon, TrashIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { filterSessionsByName } from '@/lib/sessionFilter';
import { toast } from '@/lib/toast';

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

// ActionMenu.Overlay renders through a portal, but React synthetic events still
// bubble along the REACT tree — so a click on a menu item ALSO reaches the
// enclosing session row's onSelect and silently switches the active chat. That
// turned "delete another chat" into: switch to the chat being deleted, delete
// it, then 404 on the now-missing id and land on the empty state. Every menu
// action therefore stops the event before it leaves the menu.
function menuAction(fn: () => void) {
  return (e: SyntheticEvent) => {
    e.stopPropagation();
    fn();
  };
}

function SessionItem({ s, activeId, isRunning, isAwaiting, onSelect, onPin, onFork, onDelete }: SessionItemProps): ReactElement {
  const [menuOpen, setMenuOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement>(null);
  const isActive = s.id === activeId;
  return (
    <ActionList.Item
      className="session-row"
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
        label=""
        onClick={() => setMenuOpen(o => !o)}
      />
      <ActionMenu open={menuOpen} onOpenChange={setMenuOpen} anchorRef={anchorRef as RefObject<HTMLElement>}>
        <ActionMenu.Overlay>
          <ActionList>
            <ActionList.Item onSelect={menuAction(() => onPin(s.id, !s.pinned))}>
              <ActionList.LeadingVisual>
                {s.pinned ? <PinSlashIcon size={16} /> : <PinIcon size={16} />}
              </ActionList.LeadingVisual>
              {s.pinned ? 'Unpin' : 'Pin'}
            </ActionList.Item>
            <ActionList.Item onSelect={menuAction(() => onFork(s.id))}>
              <ActionList.LeadingVisual><RepoForkedIcon size={16} /></ActionList.LeadingVisual>
              Fork
            </ActionList.Item>
            <ActionList.Divider />
            <ActionList.Item variant="danger" onSelect={menuAction(() => onDelete(s.id))}>
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
  const { data: sessions, reload, mutateData } = useApi(() => api.sessions.list() as Promise<Session[]>);

  useEffect(() => {
    if (reloadKey) reload(); // auto-refresh: does not throw
  }, [reloadKey, reload]);
  const [creating, setCreating] = useState(false);
  const [query, setQuery] = useState('');

  // For every mutation: optimistically update the cached list AND migrate active
  // state as soon as the server call succeeds, then reconcile with a background
  // reload. The optimistic list update means a reload failure can't strand a
  // deleted session in the sidebar, hide a just-created one, or show a stale pin.
  const handleCreate = async () => {
    setCreating(true);
    try {
      const sess = await api.sessions.create('New Chat') as Session;
      mutateData(prev => (prev ? [sess, ...prev] : [sess]));
      onSelect(sess.id);
      if (onCreated) onCreated();
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Could not create chat');
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async (id: string) => {
    try {
      await api.sessions.delete(id);
    } catch (e) {
      toast.error((e as Error).message || 'Could not delete chat');
      return;
    }
    mutateData(prev => (prev ? prev.filter(s => s.id !== id) : prev));
    if (onDeleteNotify) onDeleteNotify(id);
    if (activeId === id) onSelect(null);
    reload();
  };

  const handleFork = async (id: string) => {
    try {
      const forked = await api.sessions.fork(id) as Session;
      mutateData(prev => (prev ? [forked, ...prev] : [forked]));
      onSelect(forked.id);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Could not fork chat');
    }
  };

  const handlePin = async (id: string, pinned: boolean) => {
    try {
      await api.sessions.pin(id, pinned);
    } catch (e) {
      toast.error((e as Error).message || 'Could not update pin');
      return;
    }
    mutateData(prev => (prev ? prev.map(s => (s.id === id ? { ...s, pinned } : s)) : prev));
    reload();
  };

  // Search filters before the pinned/recents split so both groups narrow
  // together and the group-collapse logic below keeps working.
  const visible = sessions ? filterSessionsByName(sessions, query) : [];
  const pinned = visible.filter(s => s.pinned);
  const recents = visible.filter(s => !s.pinned);
  const loaded = sessions !== null;
  const emptyText = query.trim() ? 'No matching chats' : 'No conversations yet';

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
        <TextInput
          className="sidebar-search"
          size="medium"
          leadingVisual={SearchIcon}
          placeholder="Search"
          aria-label="Search"
          value={query}
          onChange={e => setQuery(e.target.value)}
        />
        <IconButton
          icon={PlusIcon}
          variant="invisible"
          aria-label="New"
          onClick={handleCreate}
          disabled={creating}
        />
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
                  : <div className="blankslate">{emptyText}</div>
                }
              </ActionList.Group>
            ) : (
              recents.length > 0
                ? recents.map(renderItem)
                : <div className="blankslate">{emptyText}</div>
            )}
          </ActionList>
        )}
      </div>
    </>
  );
}
