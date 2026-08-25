import React, { useState, useEffect, useCallback } from 'react';
import { Button, TextInput, Label, Select, Checkbox, FormControl, Stack, ToggleSwitch } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { TokenListInput } from '@/components/TokenListInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, OwnerBadge, RowActionsMenu, ScopeBadge } from '@/components/CrudPanel';
import { ReadOnlyContext, canDeleteRow, canDemoteRow, canEditRow } from '@/lib/access';
import { useOwnerLabels } from '@/lib/owners';
import { useMe } from '@/lib/me';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { toast } from '@/lib/toast';

const AUTH_MODES = [
  { value: '', label: 'None' },
  { value: 'header', label: 'Static headers' },
  { value: 'oauth', label: 'OAuth' },
] as const;

interface McpServerConfig {
  endpoint?: string;
  headers?: Record<string, string>;
  auth_mode?: string;
  oauth_client_id?: string;
  oauth_client_secret?: string;
  oauth_scopes?: string;
  max_retry_attempts?: number;
  retry_backoff_ms?: number;
  use_structured_content?: boolean;
}

// Lifecycle status derived by the backend — the panel renders it verbatim and
// keeps no state model of its own (POST /connect + polling move it along).
type McpStatus = 'disabled' | 'connecting' | 'authorizing' | 'needs_auth' | 'disconnected' | 'connected';

interface McpServer {
  id: string | number;
  name: string;
  enabled: boolean;
  status: McpStatus;
  has_oauth_token?: boolean;
  config?: McpServerConfig;
  scope?: string;
  owner_id?: string;
}

interface McpFormData {
  name: string;
  enabled: boolean;
  endpoint: string;
  headers: string;
  auth_mode: string;
  oauth_client_id: string;
  oauth_client_secret: string;
  oauth_scopes: string;
  max_retry_attempts: number;
  retry_backoff_ms: number;
  use_structured_content: boolean;
}

interface McpFormProps {
  initial?: McpServer;
  onSave: (data: Partial<McpServer>) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
  onClearAuth?: () => Promise<boolean>;
}

function flatten(s: Partial<McpServer>): McpFormData {
  const c = s.config || {};
  return {
    name: s.name || '', enabled: s.enabled !== false,
    endpoint: c.endpoint || '',
    headers: c.headers ? JSON.stringify(c.headers) : '',
    auth_mode: c.auth_mode || '',
    oauth_client_id: c.oauth_client_id || '',
    oauth_client_secret: c.oauth_client_secret || '',
    oauth_scopes: c.oauth_scopes || '',
    max_retry_attempts: c.max_retry_attempts || 0,
    retry_backoff_ms: c.retry_backoff_ms || 0,
    use_structured_content: c.use_structured_content || false,
  };
}

// Throws on invalid JSON in the Args / Headers fields so the caller can block
// the save and surface it — parsing to an empty value and saving anyway
// silently discarded whatever the user typed.
function pack(form: McpFormData): Partial<McpServer> {
  const base: Partial<McpServer> = { name: form.name, enabled: form.enabled };
  const config: McpServerConfig = { endpoint: form.endpoint };
  if (form.auth_mode === 'header' || !form.auth_mode) {
    const headersRaw = form.headers.trim();
    if (headersRaw) {
      let parsed: unknown;
      try { parsed = JSON.parse(headersRaw); }
      catch { throw new Error('Headers is not valid JSON — fix or clear it before saving'); }
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Headers must be a JSON object, e.g. {"Authorization": "Bearer <token>"}');
      if (Object.keys(parsed).length > 0) config.headers = parsed as Record<string, string>;
    }
  }
  if (form.auth_mode === 'oauth') {
    config.auth_mode = 'oauth';
    if (form.oauth_client_id) config.oauth_client_id = form.oauth_client_id;
    if (form.oauth_client_secret) config.oauth_client_secret = form.oauth_client_secret;
    if (form.oauth_scopes) config.oauth_scopes = form.oauth_scopes;
  } else if (form.auth_mode === 'header') {
    config.auth_mode = 'header';
  }
  if (form.max_retry_attempts) config.max_retry_attempts = form.max_retry_attempts;
  if (form.retry_backoff_ms) config.retry_backoff_ms = form.retry_backoff_ms;
  if (form.use_structured_content) config.use_structured_content = true;
  return { ...base, config };
}

function McpForm({ initial, onSave, onCancel, onDelete, saving, onClearAuth }: McpFormProps) {
  const [form, setForm] = useState<McpFormData>(flatten(initial || {}));
  const [authCleared, setAuthCleared] = useState(false);
  const [clearing, setClearing] = useState(false);
  const set = (k: keyof McpFormData, v: string | boolean | number) => setForm(prev => ({ ...prev, [k]: v }));
  const isOAuth = form.auth_mode === 'oauth';
  const isHeader = form.auth_mode === 'header';
  const canClearAuth = !!onClearAuth && !authCleared && isOAuth && !!initial?.has_oauth_token;

  const handleClearAuth = async () => {
    if (!onClearAuth || clearing) return;
    setClearing(true);
    const ok = await onClearAuth();
    setClearing(false);
    if (ok) setAuthCleared(true);
  };

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} />)}
      {fc('Endpoint', <TextInput block value={form.endpoint} onChange={e => set('endpoint', e.target.value)} placeholder="http://localhost:3000/mcp" />)}
      {fc('Authentication',
        <Select value={form.auth_mode} onChange={e => set('auth_mode', e.target.value)}>
          {AUTH_MODES.map(m => <Select.Option key={m.value} value={m.value}>{m.label}</Select.Option>)}
        </Select>,
      )}
      {isHeader && <JsonField label="Headers (JSON object)" value={form.headers} onChange={v => set('headers', v)} placeholder='{"Authorization": "Bearer <token>"}' caption="Sent with every request, e.g. an auth or API-key header. Leave empty for none." />}
      {isOAuth && fc('Client ID',
        <TextInput block value={form.oauth_client_id} onChange={e => set('oauth_client_id', e.target.value)} placeholder="Leave empty for dynamic registration" monospace />,
        'Pre-registered OAuth client ID. Leave empty to use dynamic client registration (DCR).',
      )}
      {isOAuth && form.oauth_client_id && fc('Client secret',
        <SecretInput block value={form.oauth_client_secret} onChange={e => set('oauth_client_secret', e.target.value)} monospace />,
      )}
      {isOAuth && fc('Scopes',
        <TokenListInput ariaLabel="OAuth scopes" placeholder="read write"
          values={form.oauth_scopes.split(/\s+/).filter(Boolean)}
          onChange={vals => set('oauth_scopes', vals.join(' '))} />,
        'OAuth scopes to request.',
      )}
      {canClearAuth && fc('Saved authorization',
        <Button onClick={handleClearAuth} variant="danger" disabled={clearing}>Clear auth</Button>,
        'Disconnects and deletes the saved OAuth token; the next connect asks for authorization again.',
      )}
      {fc('Max retry attempts', <TextInput block type="number" min={0} value={String(form.max_retry_attempts || 0)} onChange={e => set('max_retry_attempts', parseInt(e.target.value) || 0)} />, '0 = no retries, -1 = retry indefinitely on a failed list_tools/call_tool')}
      {form.max_retry_attempts !== 0 && fc('Retry backoff (ms)', <TextInput block type="number" min={0} value={String(form.retry_backoff_ms || 0)} onChange={e => set('retry_backoff_ms', parseInt(e.target.value) || 0)} />, 'Base delay for exponential backoff (0 = default 1000ms)')}
      <FormControl>
        <Checkbox checked={form.use_structured_content} onChange={e => set('use_structured_content', e.target.checked)} />
        <FormControl.Label>Use structured content</FormControl.Label>
        <FormControl.Caption>Use a tool result's structuredContent field exclusively (for servers that only populate it)</FormControl.Caption>
      </FormControl>
      <FormControl>
        <Checkbox checked={form.enabled} onChange={e => set('enabled', e.target.checked)} />
        <FormControl.Label>Enabled</FormControl.Label>
      </FormControl>
      <FormActions
        saving={saving}
        onSave={() => {
          let packed: Partial<McpServer>;
          try { packed = pack(form); }
          catch (e) { toast.error((e as Error).message); return; }
          onSave(packed);
        }}
        onCancel={onCancel}
        onDelete={onDelete}
      />
    </Stack>
  );
}

const STATUS_DOT: Record<McpStatus, string> = {
  connected: 'var(--fgColor-success)',
  connecting: 'var(--fgColor-attention, var(--fgColor-muted))',
  authorizing: 'var(--fgColor-attention, var(--fgColor-muted))',
  needs_auth: 'var(--fgColor-attention, var(--fgColor-muted))',
  disconnected: 'var(--fgColor-danger, var(--fgColor-muted))',
  disabled: 'var(--fgColor-muted)',
};

// The action button each status offers; connected and disabled offer none.
// connecting is disabled (a concurrent connect would just error with
// "already in progress"), but authorizing stays CLICKABLE: the wait is on the
// user finishing a popup they may have closed, and re-clicking supersedes the
// stale attempt server-side (OAuthCoordinator.supersedeInflight) and opens a
// fresh popup — otherwise a closed popup pins the row for the full 5-minute
// pending timeout (design invariant).
const STATUS_ACTION: Partial<Record<McpStatus, { label: string; inProgress?: boolean }>> = {
  disconnected: { label: 'Connect' },
  needs_auth: { label: 'Authorize' },
  connecting: { label: 'Connecting...', inProgress: true },
  authorizing: { label: 'Authorizing... (retry)' },
};

function EnabledToggle({ server, onToggle }: { server: McpServer; onToggle: (s: McpServer) => void }) {
  const [pending, setPending] = useState(false);
  const labelId = `mcp-enabled-${server.id}`;
  const handleClick = async () => {
    if (pending) return;
    setPending(true);
    await onToggle(server);
    setPending(false);
  };
  return (
    <>
      <span id={labelId} className="sr-only">{server.enabled ? 'Disable' : 'Enable'} {server.name}</span>
      <ToggleSwitch
        checked={server.enabled}
        onClick={handleClick}
        disabled={pending}
        size="small"
        aria-labelledby={labelId}
      />
    </>
  );
}

// After a mutation the backend (re)connects in the background, so the response
// status may not have caught up yet ("disconnected" an instant before the
// handshake starts). Poll through this grace window until the list stabilizes.
const MUTATION_GRACE_MS = 8000;
const POLL_INTERVAL_MS = 1500;

export function McpServerPanel() {
  const { me } = useMe();
  const isAdmin = me?.role === 'admin';
  const { labelFor } = useOwnerLabels();
  const rowEditable = (s: McpServer) => canEditRow(isAdmin, me?.id, s);
  const { items: servers, reload, adding, editing, startAdd, startEdit, cancel, save, saving, remove } = useCrud<McpServer, Partial<McpServer>>(api.mcpServers);
  // busy covers only the POST /connect round-trip; every longer-lived state
  // (connecting, authorizing) is reported by the backend via status.
  const [busy, setBusy] = useState<Record<string | number, boolean>>({});
  const [graceUntil, setGraceUntil] = useState(0);
  const bumpGrace = useCallback(() => setGraceUntil(Date.now() + MUTATION_GRACE_MS), []);

  // One poll loop for the whole panel: run while any server is in a
  // transitional status or a recent mutation may still be settling.
  const transitional = servers.some(s => s.status === 'connecting' || s.status === 'authorizing');
  useEffect(() => {
    if (!transitional && !graceUntil) return;
    const interval = setInterval(() => {
      reload();
      if (graceUntil && Date.now() >= graceUntil) setGraceUntil(0);
    }, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [transitional, graceUntil, reload]);

  // The OAuth popup notifies us when its flow ends (success or denial); the
  // poll loop above is the fallback when the message never arrives.
  useEffect(() => {
    const handler = (event: MessageEvent) => {
      if (event.data && event.data.type === 'mcp-oauth-done') {
        bumpGrace();
        reload();
      }
    };
    window.addEventListener('message', handler);
    return () => window.removeEventListener('message', handler);
  }, [reload, bumpGrace]);

  const handleConnect = async (id: string | number) => {
    setBusy(prev => ({ ...prev, [id]: true }));
    try {
      const res = await api.mcpServers.connect(id) as { status?: string; authorize_url?: string } | null;
      if (res && res.status === 'authorization_required' && res.authorize_url) {
        window.open(res.authorize_url, 'mcp_oauth', 'width=520,height=640,popup=yes');
      }
    } catch (e: unknown) {
      toast.error((e as Error).message || 'Connect failed');
    }
    setBusy(prev => ({ ...prev, [id]: false }));
    bumpGrace();
    reload();
  };
  const handleClearAuth = async (id: string | number): Promise<boolean> => {
    try {
      await api.mcpServers.clearOAuth(id);
      toast.success('Authorization cleared');
      reload();
      return true;
    } catch (e) {
      toast.error((e as Error).message);
      return false;
    }
  };

  const handleToggleEnabled = async (s: McpServer) => {
    try {
      await api.mcpServers.update(s.id, { ...s, enabled: !s.enabled });
      bumpGrace();
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const handleSave = async (data: Partial<McpServer>) => {
    await save(data);
    bumpGrace();
  };

  const form = adding ? <McpForm saving={saving} onSave={handleSave} onCancel={cancel} />
    : editing ? <McpForm saving={saving} initial={editing} onSave={handleSave} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} onClearAuth={() => handleClearAuth(editing.id)} />
    : null;

  return (
    // Scoped rows: the form is a disabled view exactly when the opened row is
    // not the caller's to edit (canEditRow), not for every member.
    <ReadOnlyContext value={!!editing && !rowEditable(editing)}>
      <CrudPanel title="MCP Servers" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={servers.length === 0} empty="No MCP servers configured."
        onDelete={editing && canDeleteRow(isAdmin, me?.id, editing)
          ? async () => { if (await remove(editing.id, editing.name)) cancel(); } : null}>
        {servers.map(s => {
          const action = STATUS_ACTION[s.status];
          const editable = rowEditable(s);
          return (
            <ResourceRow key={s.id}
              status={<span className="form-status-dot" style={{ background: STATUS_DOT[s.status] || 'var(--fgColor-muted)' }} />}
              title={s.name}
              badges={<>
                <ScopeBadge row={s} meId={me?.id} /><OwnerBadge row={s} meId={me?.id} labelFor={labelFor} />
                {s.config && s.config.auth_mode === 'oauth' && <Label variant={BADGE.type}>OAuth</Label>}
              </>}
              sub={(s.config && s.config.endpoint) || ''}
              actions={<>
                {action && editable && (
                  <Button
                    onClick={() => handleConnect(s.id)}
                    disabled={action.inProgress || busy[s.id]}
                    size="small"
                    style={{ color: 'var(--fgColor-success)', minWidth: 90, textAlign: 'center' }}
                  >{busy[s.id] ? '...' : action.label}</Button>
                )}
                {/* Connecting arms a shared credential, so on a global row it
                    stays the admin's act — tell the member whose move it is
                    instead of showing a dead status dot. Hidden while a flow
                    is mid-flight, and off private rows (their owner's move). */}
                {action && !editable && s.scope === 'global' &&
                  s.status !== 'connecting' && s.status !== 'authorizing' && (
                  <span className="resource-row-sub">waiting for an admin to reconnect</span>
                )}
                {editable && <EnabledToggle server={s} onToggle={handleToggleEnabled} />}
                <RowActionsMenu name={s.name} editReadOnly={!editable} onEdit={() => startEdit(s)}
                  scope={{ row: s, setScope: api.mcpServers.setScope, canPromote: isAdmin, canDemote: canDemoteRow(isAdmin, me?.id, s), onDone: reload }} />
              </>}
            />
          );
        })}
      </CrudPanel>
    </ReadOnlyContext>
  );
}

export default McpServerPanel;
