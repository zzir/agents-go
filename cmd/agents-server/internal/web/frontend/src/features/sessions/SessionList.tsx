import './sessions.css';
import { useState, useEffect, useRef, type FormEvent, type ReactElement, type RefObject, type SyntheticEvent } from 'react';
import { ActionList, ActionMenu, Dialog, FormControl, IconButton, TextInput, useConfirm } from '@primer/react';
import { KebabHorizontalIcon, PencilIcon, PinIcon, PinSlashIcon, PlusIcon, RepoForkedIcon, SearchIcon, TrashIcon, WorkflowIcon, XIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi, useDebouncedValue } from '@/lib/hooks';
import type { SessionStatus } from '@/lib/protocol';
import { filterSessionsByName } from '@/lib/sessionFilter';
import { SESSION_PAGE, hasMoreSessions, sessionListKey } from '@/lib/sessionPages';
import { toast } from '@/lib/toast';

interface Session {
  id: string;
  name: string;
  pinned: boolean;
  // The status the server derived when the list was read; absent on a row a
  // mutation returned.
  status?: SessionStatus;
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
  onRename: (s: Session) => void;
  onFork: (id: string) => void;
  onDelete: (id: string) => void;
}

interface SessionListProps {
  activeId: string | null;
  onSelect: (id: string | null) => void;
  onDelete?: (id: string) => void;
  // A rename landed: the app patches the open conversation's title.
  onRenamed?: (id: string, name: string) => void;
  // New: the app opens an empty composer; the first message makes the
  // conversation (invariant 69).
  onNew: () => void;
  reloadKey: unknown;
  // Each conversation's status as session.status last announced it; a row
  // not named here shows the status the list carried (invariant 3).
  statuses?: Record<string, SessionStatus>;
  // The two places in the sidebar that are not a conversation sit with the
  // list's controls, not in the list.
  onOpenHub: () => void;
}

// A menu item's click bubbles along the React tree (through the portal) to the
// row's onSelect, which would switch the active chat; every menu action stops it.
function menuAction(fn: () => void) {
  return (e: SyntheticEvent) => {
    e.stopPropagation();
    fn();
  };
}

function SessionItem({ s, activeId, isRunning, isAwaiting, onSelect, onPin, onRename, onFork, onDelete }: SessionItemProps): ReactElement {
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
      {/* The bars are color alone; the words reach a screen reader here. */}
      {isAwaiting && <span className="sr-only"> — awaiting your approval</span>}
      {isRunning && !isAwaiting && <span className="sr-only"> — running</span>}
      {/* TrailingAction renders as a sibling of the item's button inside the
          <li>, unlike TrailingVisual which would nest a button in a button. */}
      <ActionList.TrailingAction
        ref={anchorRef}
        className="session-kebab"
        icon={KebabHorizontalIcon}
        label={`Actions for ${s.name}`}
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
            <ActionList.Item onSelect={menuAction(() => onRename(s))}>
              <ActionList.LeadingVisual><PencilIcon size={16} /></ActionList.LeadingVisual>
              Rename
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

// RenameDialog takes the new name; Enter saves like the footer button.
function RenameDialog({ session, onClose, onRenamed }: { session: Session; onClose: () => void; onRenamed: (id: string, name: string) => void }) {
  const [name, setName] = useState(session.name);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const trimmed = name.trim();
  const save = async () => {
    if (busy || !trimmed) return;
    setBusy(true);
    try {
      await api.sessions.update(session.id, trimmed);
      onRenamed(session.id, trimmed);
    } catch (e) {
      toast.error((e as Error).message || 'Could not rename session');
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Rename session"
      onClose={onClose}
      width="medium"
      initialFocusRef={inputRef}
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: busy ? 'Saving…' : 'Save', disabled: busy || !trimmed || trimmed === session.name, onClick: () => { void save(); } },
      ]}
    >
      <form onSubmit={(e: FormEvent) => { e.preventDefault(); void save(); }}>
        <FormControl required>
          <FormControl.Label>Name</FormControl.Label>
          <TextInput ref={inputRef} block value={name} onChange={e => setName(e.target.value)} />
        </FormControl>
      </form>
    </Dialog>
  );
}

export function SessionList({ activeId, onSelect, onDelete: onDeleteNotify, onRenamed: onRenamedNotify, onNew, reloadKey, statuses, onOpenHub }: SessionListProps): ReactElement {
  const confirmDialog = useConfirm();
  const [query, setQuery] = useState('');
  // The server searches the name and the first user message once the typing
  // settles; until then the rows in hand are narrowed by name.
  const q = useDebouncedValue(query.trim(), 250);
  // How much of the list is shown: a page, then one more per scroll to the
  // end. A longer prefix, not a next page, so a refresh keeps it whole.
  const [limit, setLimit] = useState(SESSION_PAGE);
  useEffect(() => { setLimit(SESSION_PAGE); }, [q]);
  const { data: sessions, loading, reload, mutateData } = useApi(
    () => api.sessions.list({ limit, q: q || undefined }) as Promise<Session[]>, [limit, q], sessionListKey(limit, q));
  const hasMore = !!sessions && hasMoreSessions(sessions, limit);
  const showMore = () => { if (hasMore && !loading) setLimit(l => l + SESSION_PAGE); };
  // The end of the list asks for more as it scrolls into view.
  const moreRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = moreRef.current;
    if (!el || !hasMore || typeof IntersectionObserver === 'undefined') return;
    const io = new IntersectionObserver(entries => { if (entries.some(e => e.isIntersecting)) showMore(); });
    io.observe(el);
    return () => io.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hasMore, loading, limit]);

  useEffect(() => {
    if (reloadKey) reload(); // auto-refresh: does not throw
  }, [reloadKey, reload]);
  // The search box is a button until clicked; it stays open while a filter is
  // typed, so the narrowed list is never shown without the query that made it.
  const [searchOpen, setSearchOpen] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (searchOpen) searchRef.current?.focus();
  }, [searchOpen]);
  const closeSearch = () => {
    setQuery('');
    setSearchOpen(false);
  };
  const [renaming, setRenaming] = useState<Session | null>(null);

  // Every mutation updates the cached list optimistically and migrates active state
  // on success, then reconciles by a background reload (whose failure strands nothing).

  // The heaviest delete in the app — the conversation goes with its messages,
  // traces and tasks — so it confirms like every other one (invariant 41).
  const handleDelete = async (id: string) => {
    const name = (sessions || []).find(s => s.id === id)?.name || id.slice(0, 8);
    const ok = await confirmDialog({
      title: `Delete “${name}”?`,
      content: 'The session is removed with its messages, traces and tasks. This cannot be undone.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    });
    if (!ok) return;
    try {
      await api.sessions.delete(id);
    } catch (e) {
      toast.error((e as Error).message || 'Could not delete session');
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
      toast.error((e as Error).message || 'Could not fork session');
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

  // The server does not announce a rename over the socket: the list and the
  // open conversation's title are patched here, then reconciled.
  const handleRenamed = (id: string, name: string) => {
    setRenaming(null);
    mutateData(prev => (prev ? prev.map(s => (s.id === id ? { ...s, name } : s)) : prev));
    if (onRenamedNotify) onRenamedNotify(id, name);
    reload();
  };

  // Search filters before the pinned/recents split so both groups narrow
  // together.
  const visible = sessions ? filterSessionsByName(sessions, query) : [];
  const pinned = visible.filter(s => s.pinned);
  const recents = visible.filter(s => !s.pinned);
  const loaded = sessions !== null;
  const emptyText = query.trim() ? 'No matching sessions' : 'No sessions yet';

  const renderItem = (s: Session) => {
    const status = statuses?.[s.id] ?? s.status;
    return (
      <SessionItem
        key={s.id}
        s={s}
        activeId={activeId}
        isRunning={status === 'running'}
        isAwaiting={status === 'requires_action'}
        onSelect={onSelect}
        onPin={handlePin}
        onRename={setRenaming}
        onFork={handleFork}
        onDelete={handleDelete}
      />
    );
  };

  return (
    <>
      <div className="sidebar-actions">
        {searchOpen ? (
          <TextInput
            ref={searchRef}
            className="sidebar-search"
            size="medium"
            leadingVisual={SearchIcon}
            placeholder="Search"
            aria-label="Search"
            value={query}
            onChange={e => setQuery(e.target.value)}
            onBlur={() => { if (!query.trim()) setSearchOpen(false); }}
            onKeyDown={e => { if (e.key === 'Escape') closeSearch(); }}
            trailingAction={query ? <TextInput.Action icon={XIcon} aria-label="Clear search" onClick={closeSearch} /> : undefined}
          />
        ) : (
          <IconButton
            icon={SearchIcon}
            variant="invisible"
            aria-label="Search"
            onClick={() => setSearchOpen(true)}
          />
        )}
        <IconButton
          className="sidebar-hub"
          icon={WorkflowIcon}
          variant="invisible"
          aria-label="Workflows"
          onClick={onOpenHub}
        />
        <IconButton
          icon={PlusIcon}
          variant="invisible"
          aria-label="New"
          onClick={onNew}
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
        {hasMore && (
          <div ref={moreRef} className="sidebar-more">
            <button type="button" className="sidebar-more-button" onClick={showMore} disabled={loading}>
              {loading ? 'Loading…' : 'Show more'}
            </button>
          </div>
        )}
      </div>
      {renaming && <RenameDialog session={renaming} onClose={() => setRenaming(null)} onRenamed={handleRenamed} />}
    </>
  );
}
