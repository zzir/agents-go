import { useState } from 'react';
import { Button, TextInput, Label, SegmentedControl, Stack } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu, ScopeBadge } from '@/components/CrudPanel';
import { ReadOnlyContext, canDeleteRow, canDemoteRow, canEditRow } from '@/lib/access';
import { useMe } from '@/lib/me';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc, seg } from '@/lib/form';
import { toast } from '@/lib/toast';
import { PROVIDERS, providerMeta, providerFacts, type ProviderTypeInfo } from '@/lib/providers';

// A configured endpoint and its credential. Agents and provider routes point
// here by id, so this panel is the one place a model-API key is entered.
interface Provider {
  id: string;
  name: string;
  type?: string;
  auth_mode?: string;
  api_key?: string;
  base_url?: string;
  chatgpt_logged_in?: boolean;
  scope?: string;
  owner_id?: string;
}

interface ProviderFormData {
  name: string;
  type: string;
  auth_mode: string;
  api_key: string;
  base_url: string;
}

const EMPTY: ProviderFormData = { name: '', type: '', auth_mode: '', api_key: '', base_url: '' };

interface ProviderFormProps {
  initial?: ProviderFormData | null;
  onSave: (form: ProviderFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  saving?: boolean;
  providerTypes: ProviderTypeInfo[] | null;
}

function ProviderForm({ initial, onSave, onCancel, onDelete, saving, providerTypes }: ProviderFormProps) {
  const [form, setForm] = useState<ProviderFormData>(initial || EMPTY);
  const set = (k: keyof ProviderFormData, v: string) => setForm(prev => ({ ...prev, [k]: v }));

  const meta = providerMeta(form.type);
  const authModesFor = (value: string) =>
    providerFacts(providerTypes, value)?.auth_modes ?? (providerMeta(value).type === 'openai' ? ['chatgpt_login'] : []);
  const supportsChatGPT = authModesFor(form.type).includes('chatgpt_login');

  // A stored key belongs to the destination it was stored for, so the server
  // refuses to restore the mask across a change of backend or endpoint. Say so
  // before the save fails.
  const initialType = initial?.type ?? '';
  const initialBaseURL = initial?.base_url ?? '';
  const destinationChanged = initial !== undefined && initial !== null &&
    (providerMeta(form.type).type !== providerMeta(initialType).type ||
      form.base_url.replace(/\/+$/, '') !== initialBaseURL.replace(/\/+$/, ''));
  const staleKeyHint = destinationChanged && form.api_key === '********'
    ? 'The stored key belongs to the previous destination — enter a new one or clear it'
    : undefined;

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. DeepSeek" />,
        'How agents pick this endpoint')}

      {seg('Backend', meta.value, PROVIDERS.map(p => [p.value, p.label] as const), v => {
        // An auth mode the new backend does not offer is cleared, so the save
        // is not rejected for a control that is no longer shown.
        setForm(prev => {
          const next = { ...prev, type: v };
          if (prev.auth_mode && !authModesFor(v).includes(prev.auth_mode)) next.auth_mode = '';
          return next;
        });
      }, 'Which API protocol this endpoint speaks')}

      {supportsChatGPT && fc('Auth mode', <SegmentedControl aria-label="Auth mode" size="small">
        <SegmentedControl.Button selected={form.auth_mode !== 'chatgpt_login'} onClick={() => set('auth_mode', '')}>
          API Key
        </SegmentedControl.Button>
        <SegmentedControl.Button selected={form.auth_mode === 'chatgpt_login'}
          // The OAuth token only ever goes to ChatGPT, so switching to it drops
          // the API-key mode's base_url AND api_key — the server refuses either
          // (a masked key 400s pointing at a field this mode hides), and there
          // is no control left to clear them below.
          onClick={() => setForm(prev => ({ ...prev, auth_mode: 'chatgpt_login', api_key: '', base_url: '' }))}>
          ChatGPT Subscribe
        </SegmentedControl.Button>
      </SegmentedControl>, 'Choose authentication method')}

      {/* No Base URL for chatgpt_login: the account token is sent only to
          ChatGPT, never to an operator-typed host. */}
      {(!supportsChatGPT || form.auth_mode !== 'chatgpt_login') && <>
        {fc('API key', <SecretInput block value={form.api_key}
          onChange={e => set('api_key', e.target.value)} placeholder={meta.keyPlaceholder} />, staleKeyHint)}
        {fc('Base URL', <TextInput block value={form.base_url}
          onChange={e => set('base_url', e.target.value)} placeholder={meta.baseURLPlaceholder} />)}
      </>}

      <FormActions saving={saving} onSave={() => onSave(form)} onCancel={onCancel} onDelete={onDelete} />
    </Stack>
  );
}

export function ProviderPanel() {
  const { me } = useMe();
  const isAdmin = me?.role === 'admin';
  const rowEditable = (p: Provider) => canEditRow(isAdmin, me?.id, p);
  const { items: providers, adding, editing, startAdd, startEdit, cancel, save, saving, remove, reload } =
    useCrud<Provider, ProviderFormData>(api.providers);
  const { data: providerTypes } = useApi<ProviderTypeInfo[]>(() => api.providerTypes.list() as Promise<ProviderTypeInfo[]>);
  // Sign-in is a two-step manual-paste flow — there is no loopback listener to
  // catch the redirect (see the API's chatgpt.complete). handleLogin opens the
  // authorize popup and reveals the paste field; handleComplete redeems the
  // callback URL the user copies back from that popup.
  const [pasteURL, setPasteURL] = useState<Record<string, string>>({});
  const [awaiting, setAwaiting] = useState<Record<string, boolean>>({});
  const [completing, setCompleting] = useState<Record<string, boolean>>({});

  const handleLogin = async (id: string) => {
    try {
      const d = await api.chatgpt.login(id) as { authorize_url: string };
      window.open(d.authorize_url, 'chatgpt_oauth', 'width=500,height=700');
      setPasteURL(prev => ({ ...prev, [id]: '' }));
      setAwaiting(prev => ({ ...prev, [id]: true }));
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const handleComplete = async (id: string) => {
    const url = (pasteURL[id] || '').trim();
    if (!url) return;
    setCompleting(prev => ({ ...prev, [id]: true }));
    try {
      await api.chatgpt.complete(id, url);
      setAwaiting(prev => ({ ...prev, [id]: false }));
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setCompleting(prev => ({ ...prev, [id]: false }));
    }
  };

  const handleLogout = async (id: string) => {
    try {
      await api.chatgpt.logout(id);
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const toForm = (p: Provider): ProviderFormData => ({
    name: p.name, type: p.type || '', auth_mode: p.auth_mode || '',
    api_key: p.api_key || '', base_url: p.base_url || '',
  });

  const form = adding ? <ProviderForm saving={saving} onSave={save} onCancel={cancel} providerTypes={providerTypes} />
    : editing ? (
      <ProviderForm saving={saving}
        initial={toForm(editing)}
        onSave={save}
        onCancel={cancel}
        onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }}
        providerTypes={providerTypes}
      />
    ) : null;

  return (
    // Scoped rows: the form is a disabled view exactly when the opened row is
    // not the caller's to edit (canEditRow), not for every member.
    <ReadOnlyContext value={!!editing && !rowEditable(editing)}>
      <CrudPanel title="Providers" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={providers.length === 0}
        onDelete={editing && canDeleteRow(isAdmin, me?.id, editing)
          ? async () => { if (await remove(editing.id, editing.name)) cancel(); } : null}
        empty="No providers yet. A provider carries the endpoint and the API key that reaches it; an agent naming none has no credential and fails its pre-flight.">
        {providers.map(p => {
          const meta = providerMeta(p.type || '');
          const chatgpt = p.auth_mode === 'chatgpt_login';
          return (
            <ResourceRow key={p.id}
              /* Login state is STATUS, so it is the dot the MCP list uses, not
                 a Label; the Sign in/out action names the next move. */
              status={chatgpt && <span
                className="form-status-dot"
                style={{ background: p.chatgpt_logged_in ? 'var(--fgColor-success)' : 'var(--fgColor-attention)' }}
                title={p.chatgpt_logged_in ? 'ChatGPT signed in' : 'ChatGPT not signed in'}
              />}
              title={p.name}
              badges={<><ScopeBadge row={p} meId={me?.id} /><Label variant={meta.badgeVariant}>{meta.badge}</Label></>}
              sub={p.base_url || meta.baseURLPlaceholder}
              actions={<>
                {chatgpt && rowEditable(p) && (p.chatgpt_logged_in
                  ? <Button onClick={() => handleLogout(p.id)} size="small" variant="invisible">Sign out</Button>
                  : awaiting[p.id]
                    ? <Stack direction="horizontal" gap="condensed" align="center">
                        <TextInput size="small" style={{ width: 220 }}
                          aria-label="Paste the ChatGPT callback URL"
                          placeholder="Paste callback URL from popup…"
                          value={pasteURL[p.id] || ''}
                          onChange={e => setPasteURL(prev => ({ ...prev, [p.id]: e.target.value }))}
                          onKeyDown={e => { if (e.key === 'Enter') handleComplete(p.id); }} />
                        <Button size="small" variant="primary" loading={!!completing[p.id]}
                          onClick={() => handleComplete(p.id)}>Complete</Button>
                        <Button size="small" variant="invisible"
                          onClick={() => setAwaiting(prev => ({ ...prev, [p.id]: false }))}>Cancel</Button>
                      </Stack>
                    : <Button onClick={() => handleLogin(p.id)} size="small" variant="invisible">Sign in</Button>)}
                <RowActionsMenu name={p.name} editReadOnly={!rowEditable(p)} onEdit={() => startEdit(p)}
                  scope={{ row: p, setScope: api.providers.setScope, canPromote: isAdmin, canDemote: canDemoteRow(isAdmin, me?.id, p), onDone: reload }} />
              </>}
            />
          );
        })}
      </CrudPanel>
    </ReadOnlyContext>
  );
}

export default ProviderPanel;
