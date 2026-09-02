import { useCallback, useEffect, useMemo } from 'react';
import { ActionList, Label, PageHeader, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { PeopleIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { useMe } from '@/lib/me';
import { LOCAL_USER_ID, reloadDirectory } from '@/lib/owners';
import { formatTime } from '@/lib/time';
import { toast } from '@/lib/toast';
import { useLoadError } from '@/features/admin/useLoadError';

type UserRow = Omit<ApiSchemas['store.User'], 'id'> & { id: string };

// The implicit token-mode account (store.LocalUserID): not a person to manage.

const listMembers = async (): Promise<UserRow[]> =>
  ((await api.auth.users.list()) ?? []).filter(x => x.id && x.id !== LOCAL_USER_ID).map(x => ({ ...x, id: x.id || '' }));

// MembersPanel: every account and its role. An admin promotes or demotes
// anyone but themself (the server refuses a self-demotion too).
export function MembersPanel() {
  const { me } = useMe();
  const { data: users, error, reload } = useApi<UserRow[]>(listMembers, [], 'admin:users');
  useLoadError(error, 'members');
  const confirm = useConfirm();

  // The owner directory the other tables name people from follows this view:
  // refreshed on open, and after any change here.
  useEffect(() => { void reloadDirectory(); }, []);

  const patch = useCallback(async (u: UserRow, p: { role?: 'admin' | 'member'; disabled?: boolean }) => {
    try {
      await api.auth.users.patch(u.id, p);
      reload();
      void reloadDirectory();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to change the account.');
    }
  }, [reload]);

  const signOutEverywhere = useCallback(async (u: UserRow) => {
    if (!(await confirm({
      title: 'Sign out everywhere?',
      content: `Every session and personal access token of ${displayName(u)} is revoked; they sign in again.`,
      confirmButtonContent: 'Sign out',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.auth.users.revokeTokens(u.id);
      toast.success(`${displayName(u)} is signed out everywhere`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to revoke the tokens.');
    }
  }, [confirm]);

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
    {
      header: 'Role', id: 'role', width: 'auto', minWidth: 90,
      renderCell: u => (
        <span className="list-nowrap">
          <Label variant={u.role === 'admin' ? 'accent' : 'secondary'}>{u.role}</Label>
          {u.disabled_at ? <> <Label variant="danger">disabled</Label></> : null}
        </span>
      ),
    },
    { header: 'Joined', id: 'joined', width: 'auto', minWidth: 120, renderCell: u => <span className="list-nowrap">{u.created_at ? formatTime(u.created_at) : ''}</span> },
    { header: 'Last sign-in', id: 'seen', width: 'auto', minWidth: 120, renderCell: u => <span className="list-nowrap">{u.last_login_at ? formatTime(u.last_login_at) : ''}</span> },
    actionsColumn<UserRow>(u => {
      const self = u.id === me?.id;
      return (
        <RowMenu label={`Actions for ${displayName(u)}`}>
          {u.role === 'admin'
            ? <ActionList.Item disabled={self} onSelect={() => { void patch(u, { role: 'member' }); }}>
                Make member
                {self && <ActionList.Description variant="block">You cannot change your own account.</ActionList.Description>}
              </ActionList.Item>
            : <ActionList.Item onSelect={() => { void patch(u, { role: 'admin' }); }}>Make admin</ActionList.Item>}
          {u.disabled_at
            ? <ActionList.Item onSelect={() => { void patch(u, { disabled: false }); }}>Enable</ActionList.Item>
            : <ActionList.Item disabled={self} onSelect={() => { void patch(u, { disabled: true }); }}>
                Disable
                <ActionList.Description variant="block">Their credentials stop working until enabled again.</ActionList.Description>
              </ActionList.Item>}
          <ActionList.Divider />
          <ActionList.Item variant="danger" disabled={self} onSelect={() => { void signOutEverywhere(u); }}>Sign out everywhere</ActionList.Item>
        </RowMenu>
      );
    }),
  ], [me, patch, signOutEverywhere]);

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
