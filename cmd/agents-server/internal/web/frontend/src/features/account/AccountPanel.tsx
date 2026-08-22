import { useCallback, useEffect, useState } from 'react';
import { Button, Flash, FormControl, Label, PageHeader, Stack, TextInput, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { PersonIcon, TrashIcon, CopyIcon } from '@primer/octicons-react';

import { api, authConfig, logout, type ApiSchemas, type AuthConfig, type AuthUser } from '@/lib/api';
import { toast } from '@/lib/toast';

type PatView = ApiSchemas['protocol.PatView'];

// AccountPanel: who is signed in, sign out, and (OAuth mode) personal access
// tokens. In token mode the PAT section is absent — the server refuses the
// surface there, since a PAT could never authenticate against a static token.
export function AccountPanel() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [me, setMe] = useState<AuthUser | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    let stale = false;
    Promise.all([authConfig(), api.auth.me()])
      .then(([c, u]) => { if (!stale) { setCfg(c); setMe(u); } })
      .catch(() => { if (!stale) setError('Failed to load account details.'); });
    return () => { stale = true; };
  }, []);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Account</PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Actions>
          <Button onClick={() => { void logout(); }}>Sign out</Button>
        </PageHeader.Actions>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {me ? (
        <Stack direction="horizontal" align="center" gap="condensed">
          <span className="account-avatar" aria-hidden>{initialOf(me)}</span>
          <Stack gap="none">
            <span className="account-name">{me.name || me.email}</span>
            <span className="account-muted">{me.email}</span>
          </Stack>
          <Label variant={me.role === 'admin' ? 'accent' : 'secondary'}>{me.role}</Label>
        </Stack>
      ) : null}
      {me?.role === 'admin' && cfg?.mode === 'oauth' ? <AdminSection me={me} /> : null}
      {cfg?.mode === 'oauth' ? <PatSection /> : cfg ? (
        <Blankslate>
          <Blankslate.Visual><PersonIcon size={24} /></Blankslate.Visual>
          <Blankslate.Heading>Token mode</Blankslate.Heading>
          <Blankslate.Description>
            This server authenticates with its single static token. Per-user
            accounts and personal access tokens come with OAuth mode
            (<code>--auth oauth</code>).
          </Blankslate.Description>
        </Blankslate>
      ) : null}
    </Stack>
  );
}

function initialOf(u: AuthUser): string {
  const s = (u.name || u.email || '?').trim();
  return s ? s[0].toUpperCase() : '?';
}

// PatSection lists, mints and revokes personal access tokens. The minted
// plaintext is shown once, right here — it is not retrievable afterwards.
function PatSection() {
  const [pats, setPats] = useState<PatView[]>([]);
  const [name, setName] = useState('');
  const [days, setDays] = useState('');
  const [minted, setMinted] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const confirm = useConfirm();

  const reload = useCallback(() => {
    api.auth.pats.list().then(setPats).catch(() => setError('Failed to load tokens.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const create = useCallback(async () => {
    setBusy(true);
    setError('');
    try {
      const res = await api.auth.pats.create(name.trim(), days ? parseInt(days, 10) || 0 : 0);
      setMinted(res.token || '');
      setName('');
      setDays('');
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create the token.');
    } finally {
      setBusy(false);
    }
  }, [name, days, reload]);

  const remove = useCallback(async (p: PatView) => {
    if (!(await confirm({
      title: 'Revoke token?',
      content: `"${p.name}" stops working immediately. Anything using it will get 401s.`,
      confirmButtonContent: 'Revoke',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.auth.pats.delete(p.id || '');
      reload();
    } catch {
      setError('Failed to revoke the token.');
    }
  }, [confirm, reload]);

  return (
    <Stack gap="normal">
      <PageHeader role="presentation">
        <PageHeader.TitleArea variant="medium">
          <PageHeader.Title>Personal access tokens</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        A token authenticates like your session — REST and WebSocket — for
        scripts and CI. The secret is shown once, at creation.
      </p>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {minted ? (
        <Flash variant="success">
          <Stack direction="horizontal" align="center" gap="condensed">
            <code className="account-secret">{minted}</code>
            <Button
              size="small" leadingVisual={CopyIcon}
              onClick={() => { void navigator.clipboard.writeText(minted).then(() => toast.success('Copied')); }}
            >Copy</Button>
          </Stack>
          <p className="account-muted" style={{ marginTop: 4, marginBottom: 0 }}>Save it now — it cannot be shown again.</p>
        </Flash>
      ) : null}
      <Stack direction="horizontal" align="end" gap="condensed" wrap="wrap">
        <FormControl>
          <FormControl.Label>Name</FormControl.Label>
          <TextInput value={name} placeholder="ci-deploy" onChange={e => setName(e.target.value)} block />
        </FormControl>
        <FormControl>
          <FormControl.Label>Expires (days, empty = never)</FormControl.Label>
          <TextInput value={days} type="number" min={1} onChange={e => setDays(e.target.value)} block />
        </FormControl>
        <Button variant="primary" disabled={busy || !name.trim()} onClick={() => { void create(); }}>Create</Button>
      </Stack>
      {pats.length === 0 ? (
        <span className="account-muted">No tokens yet.</span>
      ) : (
        <Stack gap="none" className="account-pat-list">
          {pats.map(p => (
            <Stack key={p.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
              <span className="account-name" style={{ flexGrow: 1 }}>{p.name}</span>
              <span className="account-muted">
                created {shortDate(p.created_at)}
                {p.last_used_at ? ` · last used ${shortDate(p.last_used_at)}` : ''}
                {p.expires_at ? ` · expires ${shortDate(p.expires_at)}` : ' · never expires'}
              </span>
              <Button size="small" variant="danger" leadingVisual={TrashIcon} onClick={() => { void remove(p); }}>
                Revoke
              </Button>
            </Stack>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

type UserRow = ApiSchemas['store.User'];
type SessionRow = ApiSchemas['store.Session'];
type AuditRow = ApiSchemas['store.AuditEvent'];

// AdminSection is the admin's management view: every account with its role,
// and every owner's sessions — existence and recency only; content stays the
// owner's. Deleting is management; reading is not offered.
function AdminSection({ me }: { me: AuthUser }) {
  const [users, setUsers] = useState<UserRow[]>([]);
  const [sessions, setSessions] = useState<SessionRow[]>([]);
  const [audit, setAudit] = useState<AuditRow[]>([]);
  const [error, setError] = useState('');
  const confirm = useConfirm();

  const reload = useCallback(() => {
    Promise.all([api.auth.users.list(), api.sessions.listAll(), api.auth.audit(50)])
      .then(([u, s, a]) => { setUsers(u); setSessions(s); setAudit(a); })
      .catch(() => setError('Failed to load the admin view.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const emailOf = (ownerId?: string) => users.find(u => u.id === ownerId)?.email || ownerId || '';

  const setRole = useCallback(async (u: UserRow, role: 'admin' | 'member') => {
    try {
      await api.auth.users.setRole(u.id || '', role);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to change the role.');
    }
  }, [reload]);

  const removeSession = useCallback(async (s: SessionRow) => {
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
  }, [confirm, reload, users]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <Stack gap="normal">
      <PageHeader role="presentation">
        <PageHeader.TitleArea variant="medium">
          <PageHeader.Title>Users</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      <Stack gap="none" className="account-pat-list">
        {users.filter(u => u.id !== 'local').map(u => (
          <Stack key={u.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
            <span className="account-name" style={{ flexGrow: 1 }}>{u.name || u.email}</span>
            <span className="account-muted">{u.email}</span>
            <Label variant={u.role === 'admin' ? 'accent' : 'secondary'}>{u.role}</Label>
            {u.id === me.id ? null : (
              <Button size="small" onClick={() => { void setRole(u, u.role === 'admin' ? 'member' : 'admin'); }}>
                {u.role === 'admin' ? 'Make member' : 'Make admin'}
              </Button>
            )}
          </Stack>
        ))}
      </Stack>
      <PageHeader role="presentation">
        <PageHeader.TitleArea variant="medium">
          <PageHeader.Title>All sessions</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        Every owner's conversations, by recency. Content is the owner's alone;
        an admin may delete one.
      </p>
      {sessions.length === 0 ? (
        <span className="account-muted">No sessions.</span>
      ) : (
        <Stack gap="none" className="account-pat-list">
          {sessions.map(s => (
            <Stack key={s.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
              <span className="account-name" style={{ flexGrow: 1 }}>{s.name}</span>
              <span className="account-muted">{emailOf(s.owner_id)} · {shortDate(s.updated_at)}</span>
              <Button size="small" variant="danger" leadingVisual={TrashIcon} onClick={() => { void removeSession(s); }}>
                Delete
              </Button>
            </Stack>
          ))}
        </Stack>
      )}
      <PageHeader role="presentation">
        <PageHeader.TitleArea variant="medium">
          <PageHeader.Title>Audit log</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        Who did what: every configuration change, approval decision, run start
        and terminal opened — the latest 50. Retention is the server's
        <code> --audit-retention-days</code>, not a setting.
      </p>
      {audit.length === 0 ? (
        <span className="account-muted">Nothing recorded yet.</span>
      ) : (
        <Stack gap="none" className="account-pat-list">
          {audit.map(e => (
            <Stack key={e.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
              <code className="account-secret" style={{ flexGrow: 1 }}>{e.action}{e.resource ? ` ${e.resource}` : ''}{e.detail ? ` (${e.detail})` : ''}</code>
              <span className="account-muted">{e.actor_email || e.actor_id} · {shortTime(e.created_at)}</span>
            </Stack>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

function shortTime(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? '' : d.toLocaleString();
}

function shortDate(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? '' : d.toLocaleDateString();
}

export default AccountPanel;
