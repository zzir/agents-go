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

function shortDate(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? '' : d.toLocaleDateString();
}

export default AccountPanel;
