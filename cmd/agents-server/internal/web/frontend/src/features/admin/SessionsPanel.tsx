import { useCallback, useMemo, useState } from 'react';
import { ActionList, useConfirm } from '@primer/react';
import type { Column } from '@primer/react/experimental';
import { CommentDiscussionIcon } from '@primer/octicons-react';
import { RowMenu, actionsColumn } from '@/components/ListTable';
import { OwnerDialog } from '@/components/TransferDialog';
import { api, type ApiSchemas } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { useMe } from '@/lib/me';
import { LOCAL_USER_ID, useOwnerLabels } from '@/lib/owners';
import { formatTime } from '@/lib/time';
import { toast } from '@/lib/toast';
import { OwnerCell, ownerLabel } from '@/features/admin/OwnerCell';
import { AdminListPage } from '@/features/admin/AdminListPage';
import { SESSION_REMOVED } from '@/features/sessions/SessionPicker';

type SessionRow = Omit<ApiSchemas['store.Session'], 'id'> & { id: string };

const listSessions = async (): Promise<SessionRow[]> =>
  ((await api.sessions.listAll()) ?? []).map(row => ({ ...row, id: row.id || '' }));

// SessionsPanel: every owner's conversations — existence and recency only;
// content stays the owner's. Deleting and reassigning are management;
// reading is not offered.
export function SessionsPanel() {
  const { data: sessions, error, reload } = useApi<SessionRow[]>(listSessions, [], 'admin:sessions');
  // The Workspace column's id→name map; a failure leaves ids unnamed rather
  // than failing the listing.
  const { data: workspaces } = useApi(() => api.workspaces.listAll(), [], 'admin:workspaces');
  const workspaceNames = useMemo(() => Object.fromEntries((workspaces ?? []).map(p => [p.id || '', p.name || ''])), [workspaces]);
  const [reassigning, setReassigning] = useState<SessionRow | null>(null);
  const confirm = useConfirm();
  const { me } = useMe();
  // The shared directory, resolved at render: a listing that folded the
  // owner's label into its rows would reload the moment it arrived.
  const { users, ownerOf, labelFor } = useOwnerLabels();

  const remove = useCallback(async (s: SessionRow) => {
    if (!(await confirm({
      title: 'Delete session?',
      content: `"${s.name}" (${ownerLabel(labelFor, s.owner_id)}) and everything in it will be removed.`,
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.sessions.delete(s.id);
      // The app drops its state for the conversation as the sidebar's delete
      // would — it may be the one open.
      window.dispatchEvent(new CustomEvent(SESSION_REMOVED, { detail: s.id }));
      reload();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to delete the session.');
    }
  }, [confirm, reload, labelFor]);

  // A bound workspace always resolves (its delete is refused while sessions
  // bind it); the raw id is the honest fallback for a map that failed to load.
  const workspaceOf = useCallback((s: SessionRow) => s.workspace_id ? (workspaceNames[s.workspace_id] ?? s.workspace_id) : '', [workspaceNames]);

  const columns = useMemo<Column<SessionRow>[]>(() => [
    { header: 'Session', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 160, renderCell: s => <span className="list-clip" title={s.name}>{s.name}</span> },
    { header: 'Owner', id: 'owner', width: 'growCollapse', minWidth: 120, maxWidth: 260, renderCell: s => <OwnerCell ownerId={s.owner_id} ownerOf={ownerOf} labelFor={labelFor} /> },
    { header: 'Workspace', id: 'workspace', width: 'growCollapse', minWidth: 100, maxWidth: 220, renderCell: s => <span className="list-clip" title={workspaceOf(s)}>{workspaceOf(s)}</span> },
    { header: 'Updated', id: 'updated', width: 'auto', minWidth: 130, renderCell: s => <span className="list-nowrap">{s.updated_at ? formatTime(s.updated_at) : ''}</span> },
    actionsColumn<SessionRow>(s => (
      <RowMenu label={`Actions for ${s.name}`}>
        <ActionList.Item onSelect={() => setReassigning(s)}>Reassign…</ActionList.Item>
        <ActionList.Item variant="danger" onSelect={() => { void remove(s); }}>Delete</ActionList.Item>
      </RowMenu>
    )),
  ], [remove, ownerOf, labelFor, workspaceOf]);

  return (
    <AdminListPage
      titleId="sessions-title"
      title="Sessions"
      description="Every owner's sessions, newest first. Content is theirs alone; an admin may delete or reassign one."
      what="sessions"
      rows={sessions}
      error={error}
      onRetry={reload}
      columns={columns}
      match={(s, q) => `${s.name || ''} ${ownerLabel(labelFor, s.owner_id)} ${workspaceOf(s)}`.toLowerCase().includes(q)}
      empty={{ icon: CommentDiscussionIcon, heading: 'No sessions', text: 'Sessions list here as members start them.' }}
    >
      {/* The session and the task sessions serving it move together. */}
      {reassigning ? (
        <OwnerDialog
          title="Reassign session"
          action="Reassign"
          failure="Failed to reassign the session."
          caption={<>&quot;{reassigning.name}&quot; ({ownerLabel(labelFor, reassigning.owner_id)}) and the task sessions serving it become theirs. A session with a run in progress is refused.</>}
          users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== reassigning.owner_id)}
          move={userId => api.sessions.setOwner(reassigning.id, userId)}
          onClose={() => setReassigning(null)}
          onDone={userId => {
            // Reassigned away from this browser's account: gone like a delete.
            if (userId !== me?.id) window.dispatchEvent(new CustomEvent(SESSION_REMOVED, { detail: reassigning.id }));
            setReassigning(null);
            reload();
          }}
        />
      ) : null}
    </AdminListPage>
  );
}

export default SessionsPanel;
