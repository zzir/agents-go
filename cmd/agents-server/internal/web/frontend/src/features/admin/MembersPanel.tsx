import { useCallback, useEffect, useMemo, useState } from 'react';
import { ActionList, Flash, Label, PageHeader, Stack } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { PeopleIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas, type AuthUser } from '@/lib/api';
import { shortDate } from '@/lib/format';

type UserRow = Omit<ApiSchemas['store.User'], 'id'> & { id: string };

// The implicit token-mode account (store.LocalUserID): not a person to manage.
export const LOCAL_USER_ID = '00000000-0000-0000-0000-000000000001';

// MembersPanel: every account and its role. An admin promotes or demotes
// anyone but themself (the server refuses a self-demotion too).
export function MembersPanel() {
  const [me, setMe] = useState<AuthUser | null>(null);
  const [users, setUsers] = useState<UserRow[] | null>(null);
  const [error, setError] = useState('');

  const reload = useCallback(() => {
    Promise.all([api.auth.me(), api.auth.users.list()])
      .then(([u, list]) => {
        setMe(u);
        setUsers((list ?? []).filter(x => x.id && x.id !== LOCAL_USER_ID).map(x => ({ ...x, id: x.id || '' })));
      })
      .catch(() => setError('Failed to load members.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const setRole = useCallback(async (u: UserRow, role: 'admin' | 'member') => {
    try {
      await api.auth.users.setRole(u.id, role);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to change the role.');
    }
  }, [reload]);

  const columns = useMemo<Column<UserRow>[]>(() => [
    {
      header: 'Member', id: 'member', rowHeader: true, width: 'growCollapse', minWidth: 200,
      renderCell: u => (
        <span className="list-person">
          <UserAvatar user={u} size={24} />
          <span className="list-person-text">
            <span className="list-clip">{displayName(u)}</span>
            <span className="list-clip account-muted">{u.email}</span>
          </span>
        </span>
      ),
    },
    { header: 'Role', id: 'role', width: 'auto', renderCell: u => <Label variant={u.role === 'admin' ? 'accent' : 'secondary'}>{u.role}</Label> },
    { header: 'Joined', id: 'joined', width: 'auto', renderCell: u => <span className="list-nowrap">{shortDate(u.created_at)}</span> },
    { header: 'Last sign-in', id: 'seen', width: 'auto', renderCell: u => <span className="list-nowrap">{u.last_login_at ? shortDate(u.last_login_at) : ''}</span> },
    actionsColumn<UserRow>(u => (
      <RowMenu label={`Actions for ${displayName(u)}`}>
        {u.role === 'admin'
          ? <ActionList.Item disabled={u.id === me?.id} onSelect={() => { void setRole(u, 'member'); }}>
              Make member
              {u.id === me?.id && <ActionList.Description variant="block">You cannot demote yourself.</ActionList.Description>}
            </ActionList.Item>
          : <ActionList.Item onSelect={() => { void setRole(u, 'admin'); }}>Make admin</ActionList.Item>}
      </RowMenu>
    )),
  ], [me, setRole]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="members-title">Members</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          Everyone who has signed in. Admins write shared configuration and manage
          sessions; members run their own.
        </PageHeader.Description>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      <ListTable
        labelledBy="members-title"
        rows={users ?? []}
        columns={columns}
        loading={users === null}
        search={{ placeholder: 'Search members', match: (u, q) => `${u.name || ''} ${u.email || ''}`.toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><PeopleIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>No members yet</Blankslate.Heading>
            <Blankslate.Description>
              Accounts are created at first sign-in with OAuth mode
              (<code>--auth oauth</code>). Token mode has the one local admin.
            </Blankslate.Description>
          </Blankslate>
        )}
      />
    </Stack>
  );
}

export default MembersPanel;
