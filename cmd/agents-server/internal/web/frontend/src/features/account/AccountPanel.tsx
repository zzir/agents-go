import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { ActionList, Button, Dialog, Flash, FormControl, IconButton, Label, PageHeader, Stack, TextInput, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { KeyIcon, PersonIcon, XIcon } from '@primer/octicons-react';

import { UserAvatar, displayName } from '@/components/UserAvatar';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, authConfig, type ApiSchemas, type AuthConfig } from '@/lib/api';
import { shortDate } from '@/lib/format';
import { useCopy } from '@/lib/hooks';
import { useMe } from '@/lib/me';

type PatView = ApiSchemas['protocol.PatView'];
type PatRow = Omit<PatView, 'id'> & { id: string };

// AccountPanel: who is signed in and (OAuth mode) personal access tokens. In
// token mode the PAT section is absent — the server refuses the surface
// there, since a PAT could never authenticate against a static token.
// Signing out and administration live in the sidebar's account menu.
export function AccountPanel() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const { me, error: meError } = useMe();
  const [error, setError] = useState('');

  useEffect(() => {
    let stale = false;
    authConfig()
      .then(c => { if (!stale) setCfg(c); })
      .catch(() => { if (!stale) setError('Failed to load account details.'); });
    return () => { stale = true; };
  }, []);

  return (
    <Stack gap="spacious">
      <Stack gap="normal">
        <PageHeader>
          <PageHeader.TitleArea>
            <PageHeader.Title>Account</PageHeader.Title>
          </PageHeader.TitleArea>
          <PageHeader.Description>The account this browser is signed in as.</PageHeader.Description>
        </PageHeader>
        {error || meError ? <Flash variant="danger">{error || 'Failed to load account details.'}</Flash> : null}
        {me ? (
          <div className="account-profile">
            <UserAvatar user={me} size={40} />
            <div className="account-profile-text">
              <span className="account-name">{displayName(me)}</span>
              <span className="account-muted">{me.email}</span>
            </div>
            <Label variant={me.role === 'admin' ? 'accent' : 'secondary'}>{me.role}</Label>
          </div>
        ) : null}
      </Stack>
      {cfg?.mode === 'oauth' ? <PatSection /> : cfg ? (
        <Blankslate border>
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

// PatSection lists, mints and revokes personal access tokens. The minted
// plaintext is shown once, right here — it is not retrievable afterwards.
function PatSection() {
  const [pats, setPats] = useState<PatView[] | null>(null);
  const [minted, setMinted] = useState('');
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const confirm = useConfirm();
  const { copied, copy } = useCopy();

  const reload = useCallback(() => {
    api.auth.pats.list().then(p => { setPats(p ?? []); setError(''); }).catch(() => setError('Failed to load tokens.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const remove = useCallback(async (p: PatRow) => {
    if (!(await confirm({
      title: 'Revoke token?',
      content: `"${p.name}" stops working immediately. Anything using it will get 401s.`,
      confirmButtonContent: 'Revoke',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.auth.pats.delete(p.id);
      reload();
    } catch {
      setError('Failed to revoke the token.');
    }
  }, [confirm, reload]);

  const rows = useMemo<PatRow[]>(() => (pats ?? []).map(p => ({ ...p, id: p.id || '' })), [pats]);
  const columns: Column<PatRow>[] = [
    { header: 'Name', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 160, renderCell: p => <span className="list-clip" title={p.name}>{p.name}</span> },
    { header: 'Created', id: 'created', width: 'auto', renderCell: p => <span className="list-nowrap">{shortDate(p.created_at)}</span> },
    { header: 'Last used', id: 'used', width: 'auto', renderCell: p => <span className="list-nowrap">{p.last_used_at ? shortDate(p.last_used_at) : 'never'}</span> },
    { header: 'Expires', id: 'expires', width: 'auto', renderCell: p => <span className="list-nowrap">{p.expires_at ? shortDate(p.expires_at) : 'never'}</span> },
    actionsColumn<PatRow>(p => (
      <RowMenu label={`Actions for ${p.name}`}>
        <ActionList.Item variant="danger" onSelect={() => { void remove(p); }}>Revoke</ActionList.Item>
      </RowMenu>
    )),
  ];

  return (
    <Stack gap="normal">
      <PageHeader role="presentation">
        <PageHeader.TitleArea variant="medium">
          <PageHeader.Title as="h3"><span id="pat-title">Personal access tokens</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Actions>
          <Button size="small" variant="primary" onClick={() => setCreating(true)}>New token</Button>
        </PageHeader.Actions>
        <PageHeader.Description>
          A token authenticates like your session — REST and WebSocket — for
          scripts and CI. The secret is shown once, at creation.
        </PageHeader.Description>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {minted ? (
        <Flash variant="success">
          <Stack direction="horizontal" align="center" gap="condensed">
            <code className="account-secret">{minted}</code>
            <Button size="small" onClick={() => copy(minted)}>{copied ? 'Copied' : 'Copy'}</Button>
            <IconButton icon={XIcon} size="small" variant="invisible" aria-label="Dismiss" onClick={() => setMinted('')} />
          </Stack>
          <p className="account-muted" style={{ marginTop: 4, marginBottom: 0 }}>Save it now — it cannot be shown again.</p>
        </Flash>
      ) : null}
      <ListTable
        labelledBy="pat-title"
        rows={rows}
        columns={columns}
        loading={pats === null}
        search={{ placeholder: 'Search tokens', match: (p, q) => (p.name || '').toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><KeyIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>No tokens yet</Blankslate.Heading>
            <Blankslate.Description>Create one for a script or CI job; it signs in as you.</Blankslate.Description>
            <Blankslate.PrimaryAction onClick={() => setCreating(true)}>New token</Blankslate.PrimaryAction>
          </Blankslate>
        )}
      />
      {creating && (
        <NewTokenDialog
          onClose={() => setCreating(false)}
          onMinted={token => { setMinted(token); setCreating(false); reload(); }}
        />
      )}
    </Stack>
  );
}

// NewTokenDialog collects a name and an optional lifetime; the plaintext goes
// back to the section, which is the one place it is ever shown.
function NewTokenDialog({ onClose, onMinted }: { onClose: () => void; onMinted: (token: string) => void }) {
  const [name, setName] = useState('');
  const [days, setDays] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const nameRef = useRef<HTMLInputElement>(null);

  const create = useCallback(async () => {
    if (busy || !name.trim()) return;
    setBusy(true);
    setError('');
    try {
      const res = await api.auth.pats.create(name.trim(), days ? parseInt(days, 10) || 0 : 0);
      onMinted(res.token || '');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create the token.');
      setBusy(false);
    }
  }, [busy, name, days, onMinted]);

  return (
    <Dialog
      title="New personal access token"
      onClose={onClose}
      width="medium"
      initialFocusRef={nameRef}
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: 'Create token', disabled: busy || !name.trim(), onClick: () => { void create(); } },
      ]}
    >
      {/* A form, so Enter in a field submits like the footer button. */}
      <Stack as="form" gap="normal" onSubmit={(e: FormEvent) => { e.preventDefault(); void create(); }}>
        {error ? <Flash variant="danger">{error}</Flash> : null}
        <FormControl required>
          <FormControl.Label>Name</FormControl.Label>
          <FormControl.Caption>What the token is for — it is how you will recognise it later.</FormControl.Caption>
          <TextInput ref={nameRef} block value={name} placeholder="ci-deploy" onChange={e => setName(e.target.value)} />
        </FormControl>
        <FormControl>
          <FormControl.Label>Expires in days</FormControl.Label>
          <FormControl.Caption>Leave empty for a token that never expires.</FormControl.Caption>
          <TextInput block value={days} type="number" min={1} onChange={e => setDays(e.target.value)} />
        </FormControl>
      </Stack>
    </Dialog>
  );
}

export default AccountPanel;
