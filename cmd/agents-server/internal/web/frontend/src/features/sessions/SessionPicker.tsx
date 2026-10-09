import { useMemo, useState } from 'react';
import { Button, Flash, SelectPanel, type SelectPanelItemInput } from '@primer/react';
import { CommentDiscussionIcon, PinIcon, PlusIcon, TriangleDownIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { nameOf } from '@/lib/named';
import { filterSessionsByName } from '@/lib/sessionFilter';
import './sessions.css';

interface SessionRef { id: string; name: string; pinned?: boolean; project_id?: string }

// SESSION_REMOVED carries (detail) the id of a session this browser can no
// longer see (deleted or reassigned from Admin), for the app to drop its state.
export const SESSION_REMOVED = 'sessions:removed';

// NEW_SESSION is the picker's first row as a value: the ask for a session,
// which the form holding the picker makes when it saves (invariant 69).
export const NEW_SESSION = '__new_session__';
const NEW_SESSION_TEXT = 'New session';

// SessionPicker chooses ONE session, in the sidebar's order and search, or
// NEW_SESSION as the first row. The panel is capped at a SMALL height: an
// overlay fitting neither above nor below lands clamped at the screen's far left.
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

// UnboundHint says, under a picker, when the chosen session (a new one always)
// has no project bound, so no file or command tools; `what` names the work.
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
