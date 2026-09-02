import { useCallback, useMemo, useState } from 'react';
import { ActionList, Dialog, FormControl, PageHeader, Select, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { CommentDiscussionIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { useMe } from '@/lib/me';
import { useOwnerLabels, type UserLabel } from '@/lib/owners';
import { formatTime } from '@/lib/time';
import { toast } from '@/lib/toast';
import { LOCAL_USER_ID } from '@/lib/owners';
import { OwnerCell, ownerLabel } from '@/features/admin/OwnerCell';
import { useLoadError } from '@/features/admin/useLoadError';
import { SESSION_REMOVED } from '@/features/sessions/SessionPicker';

type SessionRow = Omit<ApiSchemas['store.Session'], 'id'> & { id: string };

const listSessions = async (): Promise<SessionRow[]> =>
  ((await api.sessions.listAll()) ?? []).map(row => ({ ...row, id: row.id || '' }));

// SessionsPanel: every owner's conversations — existence and recency only;
// content stays the owner's. Deleting and reassigning are management;
// reading is not offered.
export function SessionsPanel() {
  const { data: sessions, error, reload } = useApi<SessionRow[]>(listSessions, [], 'admin:sessions');
  useLoadError(error, 'sessions');
  // The Project column's id→name map; a failure leaves ids unnamed rather
  // than failing the listing.
  const { data: projects } = useApi(() => api.projects.listAll(), [], 'admin:projects');
  const projectNames = useMemo(() => Object.fromEntries((projects ?? []).map(p => [p.id || '', p.name || ''])), [projects]);
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

  // A bound project always resolves (its delete is refused while sessions
  // bind it); the raw id is the honest fallback for a map that failed to load.
  const projectOf = useCallback((s: SessionRow) => s.project_id ? (projectNames[s.project_id] ?? s.project_id) : '', [projectNames]);

  const columns = useMemo<Column<SessionRow>[]>(() => [
    { header: 'Session', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 160, renderCell: s => <span className="list-clip" title={s.name}>{s.name}</span> },
    { header: 'Owner', id: 'owner', width: 'growCollapse', minWidth: 120, maxWidth: 260, renderCell: s => <OwnerCell ownerId={s.owner_id} ownerOf={ownerOf} labelFor={labelFor} /> },
    { header: 'Project', id: 'project', width: 'growCollapse', minWidth: 100, maxWidth: 220, renderCell: s => <span className="list-clip" title={projectOf(s)}>{projectOf(s)}</span> },
    { header: 'Updated', id: 'updated', width: 'auto', minWidth: 130, renderCell: s => <span className="list-nowrap">{s.updated_at ? formatTime(s.updated_at) : ''}</span> },
    actionsColumn<SessionRow>(s => (
      <RowMenu label={`Actions for ${s.name}`}>
        <ActionList.Item onSelect={() => setReassigning(s)}>Reassign…</ActionList.Item>
        <ActionList.Item variant="danger" onSelect={() => { void remove(s); }}>Delete</ActionList.Item>
      </RowMenu>
    )),
  ], [remove, ownerOf, labelFor, projectOf]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="sessions-title">Sessions</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          Every owner's conversations, newest first. Content is theirs alone;
          an admin may delete or reassign one.
        </PageHeader.Description>
      </PageHeader>
      <ListTable
        labelledBy="sessions-title"
        rows={sessions ?? []}
        columns={columns}
        loading={sessions === null}
        search={{ placeholder: 'Search sessions', match: (s, q) => `${s.name || ''} ${ownerLabel(labelFor, s.owner_id)} ${projectOf(s)}`.toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><CommentDiscussionIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>No sessions</Blankslate.Heading>
            <Blankslate.Description>Conversations list here as members start them.</Blankslate.Description>
          </Blankslate>
        )}
      />
      {reassigning ? (
        <ReassignDialog
          session={reassigning}
          owner={ownerLabel(labelFor, reassigning.owner_id)}
          users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== reassigning.owner_id)}
          onClose={() => setReassigning(null)}
          onDone={userId => {
            // Reassigned away from this browser's account: gone like a delete.
            if (userId !== me?.id) window.dispatchEvent(new CustomEvent(SESSION_REMOVED, { detail: reassigning.id }));
            setReassigning(null);
            reload();
          }}
        />
      ) : null}
    </Stack>
  );
}

// ReassignDialog picks the account a session (and its task sessions) moves to.
function ReassignDialog({ session, owner, users, onClose, onDone }: {
  session: SessionRow; owner: string; users: UserLabel[]; onClose: () => void; onDone: (userId: string) => void;
}) {
  const [userId, setUserId] = useState(users[0]?.id || '');
  const [busy, setBusy] = useState(false);

  const reassign = useCallback(async () => {
    setBusy(true);
    try {
      await api.sessions.setOwner(session.id, userId);
      onDone(userId);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to reassign the session.');
      setBusy(false);
    }
  }, [session.id, userId, onDone]);

  return (
    <Dialog
      title="Reassign session"
      onClose={onClose}
      width="medium"
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: 'Reassign', disabled: busy || !userId, onClick: () => { void reassign(); } },
      ]}
    >
      <Stack gap="normal">
        <FormControl required>
          <FormControl.Label>New owner</FormControl.Label>
          <FormControl.Caption>
            &quot;{session.name}&quot; ({owner}) and the task sessions serving it become theirs. A session with a run in progress is refused.
          </FormControl.Caption>
          <Select block value={userId} onChange={e => setUserId(e.target.value)}>
            {users.length === 0 ? <Select.Option value="">No other account</Select.Option> : null}
            {users.map(u => <Select.Option key={u.id} value={u.id}>{u.name ? `${u.name} (${u.email})` : u.email}</Select.Option>)}
          </Select>
        </FormControl>
      </Stack>
    </Dialog>
  );
}

export default SessionsPanel;
