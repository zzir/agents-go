import { useCallback, useEffect, useMemo, useState } from 'react';
import { ActionList, Flash, PageHeader, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { CommentDiscussionIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';

type UserRow = ApiSchemas['store.User'];
type SessionRow = Omit<ApiSchemas['store.Session'], 'id'> & { id: string; owner: string };

// SessionsPanel: every owner's conversations — existence and recency only;
// content stays the owner's. Deleting is management; reading is not offered.
export function SessionsPanel() {
  const [sessions, setSessions] = useState<SessionRow[] | null>(null);
  const [error, setError] = useState('');
  const confirm = useConfirm();

  const reload = useCallback(() => {
    Promise.all([api.auth.users.list(), api.sessions.listAll()])
      .then(([u, s]) => {
        const email = (ownerId?: string) => (u ?? []).find((x: UserRow) => x.id === ownerId)?.email || ownerId || '';
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
          an admin may delete one.
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
    </Stack>
  );
}

export default SessionsPanel;
