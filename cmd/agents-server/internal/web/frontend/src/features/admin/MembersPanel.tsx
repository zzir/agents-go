import { useCallback, useEffect, useState } from 'react';
import { Button, Flash, Label, PageHeader, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { PeopleIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { api, type ApiSchemas, type AuthUser } from '@/lib/api';

type UserRow = ApiSchemas['store.User'];

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
      .then(([u, list]) => { setMe(u); setUsers((list ?? []).filter(x => x.id !== LOCAL_USER_ID)); })
      .catch(() => setError('Failed to load members.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const setRole = useCallback(async (u: UserRow, role: 'admin' | 'member') => {
    try {
      await api.auth.users.setRole(u.id || '', role);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to change the role.');
    }
  }, [reload]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Members</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        Everyone who has signed in. Admins write shared configuration and manage
        sessions; members run their own.
      </p>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {users && users.length === 0 ? (
        <Blankslate>
          <Blankslate.Visual><PeopleIcon size={24} /></Blankslate.Visual>
          <Blankslate.Heading>No members yet</Blankslate.Heading>
          <Blankslate.Description>
            Accounts are created at first sign-in with OAuth mode
            (<code>--auth oauth</code>). Token mode has the one local admin.
          </Blankslate.Description>
        </Blankslate>
      ) : users ? (
        <Stack gap="none" className="account-pat-list">
          {users.map(u => (
            <Stack key={u.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
              <UserAvatar user={u} size={24} />
              <span className="account-name" style={{ flexGrow: 1 }}>{displayName(u)}</span>
              <span className="account-muted">{u.email}</span>
              <Label variant={u.role === 'admin' ? 'accent' : 'secondary'}>{u.role}</Label>
              {u.id === me?.id ? null : (
                <Button size="small" onClick={() => { void setRole(u, u.role === 'admin' ? 'member' : 'admin'); }}>
                  {u.role === 'admin' ? 'Make member' : 'Make admin'}
                </Button>
              )}
            </Stack>
          ))}
        </Stack>
      ) : null}
    </Stack>
  );
}

export default MembersPanel;
