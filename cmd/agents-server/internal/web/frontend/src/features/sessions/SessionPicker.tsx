import { useMemo, useState } from 'react';
import { Button, Flash, SelectPanel, type SelectPanelItemInput } from '@primer/react';
import { CommentDiscussionIcon, PinIcon, PlusIcon, TriangleDownIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { nameOf } from '@/lib/named';
import { filterSessionsByName } from '@/lib/sessionFilter';
import './sessions.css';

interface SessionRef { id: string; name: string; pinned?: boolean; project_id?: string }

// SESSIONS_CHANGED is raised when a conversation is made outside the sidebar
// (a trigger's or a Run…'s own, on save), so the sidebar's list refetches —
// the same one-shot window event the API layer uses for a logout.
export const SESSIONS_CHANGED = 'sessions:changed';
// SESSION_REMOVED carries (detail) the id of a conversation this browser can
// no longer see — deleted or reassigned from the Admin dialog — for the app
// to drop its state the way the sidebar's own delete does.
export const SESSION_REMOVED = 'sessions:removed';

// NEW_SESSION is the picker's first row as a value: not a conversation but the
// ask for one. The form holding the picker makes it when it saves (invariant
// 69), so a cancelled form leaves nothing and no two forms share one.
export const NEW_SESSION = '__new_session__';
const NEW_SESSION_TEXT = 'New session';

// SessionPicker chooses ONE conversation the way the sidebar shows them — the
// same order (pinned first, then most recently changed first), the same
// search — or NEW_SESSION: a form that names a conversation must not send the
// person to the sidebar first. One flat list, "New session" its first row: the
// choice is short enough not to need headings. A row is one line, however
// long the name (the whole of it is the row's title).
// The panel is capped at a SMALL height, and grows only to its content: the
// anchored overlay flips above the anchor when it does not fit below, and
// when it fits neither it tries the sides — beside a block-wide anchor that
// is off the viewport's edge, and the panel lands clamped at the far left of
// the screen. A short panel fits one side or the other.
export function SessionPicker({ value, onChange, placeholder = 'Select a session…' }:
  { value: string; onChange: (id: string) => void; placeholder?: string }) {
  const { data: sessions } = useApi<SessionRef[]>(() => api.sessions.list() as Promise<SessionRef[]>);
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');

  const list = useMemo(() => sessions || [], [sessions]);
  const items = useMemo<SelectPanelItemInput[]>(() => {
    const visible = filterSessionsByName(list, filter);
    const ordered = [...visible.filter(s => s.pinned), ...visible.filter(s => !s.pinned)];
    return [
      { id: NEW_SESSION, text: NEW_SESSION_TEXT, leadingVisual: PlusIcon },
      ...ordered.map(s => {
        const text = s.name || s.id.slice(0, 8);
        return { id: s.id, text, title: text, leadingVisual: s.pinned ? PinIcon : CommentDiscussionIcon };
      }),
    ];
  }, [list, filter]);
  const selectedName = value === NEW_SESSION ? NEW_SESSION_TEXT : value ? nameOf(list, value) : '';
  const selected = value ? (items.find(i => i.id === value) || { id: value, text: selectedName }) : undefined;

  return (
    <SelectPanel
      title="Session"
      className="session-picker-list"
      renderAnchor={({ children: _children, ...anchorProps }) => (
        <Button block alignContent="start" trailingAction={TriangleDownIcon} className="session-picker-anchor"
          aria-label={'Session: ' + (selectedName || 'none picked')} title={selectedName || undefined} {...anchorProps}>
          {selectedName || placeholder}
        </Button>
      )}
      open={open}
      onOpenChange={setOpen}
      items={items}
      selected={selected}
      onSelectedChange={(item: SelectPanelItemInput | undefined) => {
        if (item?.id) onChange(String(item.id));
      }}
      onFilterChange={setFilter}
      placeholderText="Search"
      overlayProps={{ width: 'medium', maxHeight: 'small' }}
    />
  );
}

// UnboundHint says, under a picker, when the chosen conversation has no
// project (sandbox) bound — a new one has none yet: work started into it has
// no file or command tools — the one thing a person cannot see from the name,
// and the usual reason a coding workflow gets nowhere. what is the work: "the
// workflow", "the turn".
export function UnboundHint({ sessionId, what }: { sessionId: string; what: string }) {
  const isNew = sessionId === NEW_SESSION;
  const { data } = useApi<SessionRef | null>(
    () => (sessionId && !isNew ? api.sessions.get(sessionId) as Promise<SessionRef> : Promise.resolve(null)),
    [sessionId],
  );
  if (!sessionId || (!isNew && (!data || data.project_id))) return null;
  return (
    <Flash variant="warning" style={{ fontSize: 'var(--base-text-size-xs)', padding: 'var(--base-size-6) var(--base-size-8)' }}>
      {isNew ? 'A new session' : 'This session'} has no project bound — {what} will have no file or command tools. Bind
      one by sending it a message with a project picked, if the work touches files.
    </Flash>
  );
}
