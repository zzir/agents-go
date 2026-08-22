import { useCallback, useEffect, useMemo, useState } from 'react';
import { ActionList, Dialog, Flash, FormControl, PageHeader, Select, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { CommentDiscussionIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';
import { LOCAL_USER_ID } from '@/features/admin/MembersPanel';

type UserRow = Omit<ApiSchemas['store.User'], 'id'> & { id: string };
type SessionRow = Omit<ApiSchemas['store.Session'], 'id'> & { id: string; owner: string };

// SessionsPanel: every owner's conversations — existence and recency only;
// content stays the owner's. Deleting and reassigning are management;
// reading is not offered.
export function SessionsPanel() {
  const [sessions, setSessions] = useState<SessionRow[] | null>(null);
  const [users, setUsers] = useState<UserRow[]>([]);
  const [reassigning, setReassigning] = useState<SessionRow | null>(null);
  const [error, setError] = useState('');
  const confirm = useConfirm();

  const reload = useCallback(() => {
    Promise.all([api.auth.users.list(), api.sessions.listAll()])
      .then(([u, s]) => {
        const rows = (u ?? []).filter(x => x.id).map(x => ({ ...x, id: x.id || '' }));
        setUsers(rows);
        const email = (ownerId?: string) => rows.find(x => x.id === ownerId)?.email || ownerId || '';
        setSessions((s ?? []).map(row => ({ ...row, id: row.id || '', owner: email(row.owner_id) })));
      })
      .catch(() => setError('Failed to load sessions.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const remove = useCallback(async (s: SessionRow) => {
    if (!(await confirm({
      title: 'Delete session?',
      content: `"${s.name}" (${s.owner}) and everything in it will be removed.`,
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.sessions.delete(s.id);
      reload();
    } catch {
      setError('Failed to delete the session.');
    }
  }, [confirm, reload]);

  const columns = useMemo<Column<SessionRow>[]>(() => [
    { header: 'Session', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 160, renderCell: s => <span className="list-clip" title={s.name}>{s.name}</span> },
    { header: 'Owner', id: 'owner', width: 'growCollapse', minWidth: 120, maxWidth: 260, renderCell: s => <span className="list-clip" title={s.owner}>{s.owner}</span> },
    { header: 'Updated', id: 'updated', width: 'auto', renderCell: s => <span className="list-nowrap">{shortTime(s.updated_at)}</span> },
    actionsColumn<SessionRow>(s => (
      <RowMenu label={`Actions for ${s.name}`}>
        <ActionList.Item onSelect={() => setReassigning(s)}>Reassign…</ActionList.Item>
        <ActionList.Item variant="danger" onSelect={() => { void remove(s); }}>Delete</ActionList.Item>
      </RowMenu>
    )),
  ], [remove]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="sessions-title">Sessions</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          Every owner's conversations, by recency. Content is the owner's alone;
          an admin may delete one, or reassign it — the way a conversation made
          under the other auth mode reaches someone.
        </PageHeader.Description>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      <ListTable
        labelledBy="sessions-title"
        rows={sessions ?? []}
        columns={columns}
        loading={sessions === null}
        search={{ placeholder: 'Search sessions', match: (s, q) => `${s.name || ''} ${s.owner}`.toLowerCase().includes(q) }}
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
          users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== reassigning.owner_id)}
          onClose={() => setReassigning(null)}
          onDone={() => { setReassigning(null); reload(); }}
        />
      ) : null}
    </Stack>
  );
}

// ReassignDialog picks the account a session (and its task sessions) moves to.
function ReassignDialog({ session, users, onClose, onDone }: {
  session: SessionRow; users: UserRow[]; onClose: () => void; onDone: () => void;
}) {
  const [userId, setUserId] = useState(users[0]?.id || '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const reassign = useCallback(async () => {
    setBusy(true);
    setError('');
    try {
      await api.sessions.setOwner(session.id, userId);
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to reassign the session.');
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
        {error ? <Flash variant="danger">{error}</Flash> : null}
        <FormControl required>
          <FormControl.Label>New owner</FormControl.Label>
          <FormControl.Caption>
            &quot;{session.name}&quot; ({session.owner}) and the task sessions serving it become theirs. A session with a run in progress is refused.
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
