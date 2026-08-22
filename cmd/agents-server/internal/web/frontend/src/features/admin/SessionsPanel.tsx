import { useCallback, useEffect, useState } from 'react';
import { Button, Flash, PageHeader, Stack, useConfirm } from '@primer/react';
import { TrashIcon } from '@primer/octicons-react';
import { Paged } from '@/components/Paged';
import { api, type ApiSchemas } from '@/lib/api';
import { PAGE_SIZE, usePage } from '@/lib/hooks';
import { shortDate } from '@/lib/format';

type UserRow = ApiSchemas['store.User'];
type SessionRow = ApiSchemas['store.Session'];

// SessionsPanel: every owner's conversations — existence and recency only;
// content stays the owner's. Deleting is management; reading is not offered.
export function SessionsPanel() {
  const [users, setUsers] = useState<UserRow[]>([]);
  const [sessions, setSessions] = useState<SessionRow[]>([]);
  const [error, setError] = useState('');
  const confirm = useConfirm();
  const page = usePage(sessions, PAGE_SIZE);

  const reload = useCallback(() => {
    Promise.all([api.auth.users.list(), api.sessions.listAll()])
      .then(([u, s]) => { setUsers(u ?? []); setSessions(s ?? []); })
      .catch(() => setError('Failed to load sessions.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const emailOf = useCallback((ownerId?: string) => users.find(u => u.id === ownerId)?.email || ownerId || '', [users]);

  const remove = useCallback(async (s: SessionRow) => {
    if (!(await confirm({
      title: 'Delete session?',
      content: `"${s.name}" (${emailOf(s.owner_id)}) and everything in it will be removed.`,
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.sessions.delete(s.id || '');
      reload();
    } catch {
      setError('Failed to delete the session.');
    }
  }, [confirm, reload, emailOf]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Sessions</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        Every owner's conversations, by recency. Content is the owner's alone;
        an admin may delete one.
      </p>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {sessions.length === 0 ? (
        <span className="account-muted">No sessions.</span>
      ) : (
        <Paged page={page} total={sessions.length} label="Sessions pages">
          <Stack gap="none" className="account-pat-list">
            {page.items.map(s => (
              <Stack key={s.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
                <span className="account-name" style={{ flexGrow: 1 }}>{s.name}</span>
                <span className="account-muted">{emailOf(s.owner_id)} · {shortDate(s.updated_at)}</span>
                <Button size="small" variant="danger" leadingVisual={TrashIcon} onClick={() => { void remove(s); }}>
                  Delete
                </Button>
              </Stack>
            ))}
          </Stack>
        </Paged>
      )}
    </Stack>
  );
}

export default SessionsPanel;
