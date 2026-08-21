import { useCallback, useEffect, useRef, useState } from 'react';
import { Button, TextInput, Label, SegmentedControl, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc, seg } from '@/lib/form';
import { toast } from '@/lib/toast';
import { PROVIDERS, providerMeta, providerFacts, type ProviderTypeInfo } from '@/lib/providers';
import { BADGE } from '@/lib/badges';

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
  providerTypes: ProviderTypeInfo[] | null;
}

function ProviderForm({ initial, onSave, onCancel, onDelete, providerTypes }: ProviderFormProps) {
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
        {fc('API key', <TextInput block type="password" value={form.api_key}
          onChange={e => set('api_key', e.target.value)} placeholder={meta.keyPlaceholder} />, staleKeyHint)}
        {fc('Base URL', <TextInput block value={form.base_url}
          onChange={e => set('base_url', e.target.value)} placeholder={meta.baseURLPlaceholder} />)}
      </>}

      <div className="form-actions">
        <Button onClick={() => onSave(form)} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function ProviderPanel() {
  const { items: providers, adding, editing, startAdd, startEdit, cancel, save, remove, reload } =
    useCrud<Provider, ProviderFormData>(api.providers);
  const { data: providerTypes } = useApi<ProviderTypeInfo[]>(() => api.providerTypes.list() as Promise<ProviderTypeInfo[]>);
  const [signingIn, setSigningIn] = useState<Record<string, boolean>>({});
  const pollRef = useRef<Record<string, { interval: number; timeout: number }>>({});

  const stopPoll = useCallback((id: string | number) => {
    const p = pollRef.current[id];
    if (p) {
      clearInterval(p.interval);
      clearTimeout(p.timeout);
      delete pollRef.current[id];
    }
    setSigningIn(prev => ({ ...prev, [id]: false }));
  }, []);

  useEffect(() => () => {
    for (const id of Object.keys(pollRef.current)) stopPoll(id);
  }, [stopPoll]);

  // The ChatGPT callback runs on a separate localhost server, so there is no
  // postMessage from the popup — polling the status endpoint is the only
  // completion signal. A re-click supersedes the stale attempt instead of
  // leaving the user stuck when the popup was closed or denied.
  const handleLogin = async (id: string) => {
    stopPoll(id);
    setSigningIn(prev => ({ ...prev, [id]: true }));
    try {
      const d = await api.chatgpt.login(id) as { authorize_url: string };
      window.open(d.authorize_url, 'chatgpt_oauth', 'width=500,height=700');
      const interval = setInterval(async () => {
        try {
          const s = await api.chatgpt.status(id) as { logged_in: boolean };
          if (s.logged_in) { stopPoll(id); reload(); }
        } catch { /* ignore transient */ }
      }, 2000) as unknown as number;
      // Give up after 2 minutes: the button reverts to "Sign in" and a later
      // completed login still shows up on the next reload.
      const timeout = setTimeout(() => { stopPoll(id); reload(); }, 2 * 60 * 1000) as unknown as number;
      pollRef.current[id] = { interval, timeout };
    } catch (e) {
      toast.error((e as Error).message);
      setSigningIn(prev => ({ ...prev, [id]: false }));
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

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Providers</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && <ProviderForm onSave={save} onCancel={cancel} providerTypes={providerTypes} />}
      {editing && (
        <ProviderForm
          initial={toForm(editing)}
          onSave={save}
          onCancel={cancel}
          onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }}
          providerTypes={providerTypes}
        />
      )}

      {!adding && !editing && <div className="Box">
        {providers.map(p => {
          const meta = providerMeta(p.type || '');
          const chatgpt = p.auth_mode === 'chatgpt_login';
          return (
            <div key={p.id} className="Box-row">
              <div className="resource-row-main">
                <div className="resource-row-head">
                  {/* Login state is STATUS, so it is the dot the MCP list uses,
                      not a Label; the Sign in/out action names the next move. */}
                  {chatgpt && <span
                    className="form-status-dot"
                    style={{ background: p.chatgpt_logged_in ? 'var(--fgColor-success)' : 'var(--fgColor-attention)' }}
                    title={p.chatgpt_logged_in ? 'ChatGPT signed in' : 'ChatGPT not signed in'}
                  />}
                  <span className="resource-row-title">{p.name}</span>
                  <Label variant={BADGE.type}>{meta.badge}</Label>
                </div>
                <div className="resource-row-sub">{p.base_url || meta.baseURLPlaceholder}</div>
              </div>
              <div className="resource-row-actions">
                {chatgpt && (p.chatgpt_logged_in
                  ? <Button onClick={() => handleLogout(p.id)} size="small" variant="invisible">Sign out</Button>
                  : <Button onClick={() => handleLogin(p.id)} size="small" variant="invisible" loading={!!signingIn[p.id]}>Sign in</Button>)}
                <Button onClick={() => startEdit(p)} size="small" variant="invisible">Edit</Button>
              </div>
            </div>
          );
        })}
        {providers.length === 0 && (
          <Blankslate>
            <Blankslate.Description>
              No providers yet. An agent without one runs on the built-in default: the OpenAI backend
              on the global API key from Settings.
            </Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default ProviderPanel;
