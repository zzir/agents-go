import React, { useState, useEffect, useCallback } from 'react';
import { Button, TextInput, Label, Link, Select, Stack, ToggleSwitch, useConfirm } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { ToggleRow } from '@/components/ToggleRow';
import { TokenListInput } from '@/components/TokenListInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu, ScopeBadge } from '@/components/CrudPanel';
import { useScopeFilter } from '@/components/ScopeFilter';
import { useTransfer } from '@/components/TransferDialog';
import { filterRows } from '@/lib/listFilter';
import { ReadOnlyContext, canDeleteRow, canDemoteRow, canEditRow } from '@/lib/access';
import { useMe } from '@/lib/me';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { headersToText, parseHeadersText } from '@/lib/headers';
import { numberDraft, parseWholeNumber } from '@/lib/numericField';
import { toast } from '@/lib/toast';
import { listEmpty } from '@/features/settings/listEmpty';

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
  // The server's tools plan mode may call while planning, by their own names.
  read_only_tools?: string[];
}

// A tool as GET /mcp-servers/:id/tools lists it.
interface McpTool { name: string; description?: string; original_name: string; read_only_hint?: boolean }

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
  // Whole numbers, held as strings while editing (invariant 80).
  max_retry_attempts: string;
  retry_backoff_ms: string;
  use_structured_content: boolean;
  read_only_tools: string[];
}

interface McpFormProps {
  initial?: McpServer;
  onSave: (data: Partial<McpServer>) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
  onClearAuth?: () => Promise<boolean>;
}

export function flatten(s: Partial<McpServer>): McpFormData {
  const c = s.config || {};
  return {
    name: s.name || '', enabled: s.enabled !== false,
    endpoint: c.endpoint || '',
    headers: headersToText(c.headers),
    auth_mode: c.auth_mode || '',
    oauth_client_id: c.oauth_client_id || '',
    oauth_client_secret: c.oauth_client_secret || '',
    oauth_scopes: c.oauth_scopes || '',
    max_retry_attempts: numberDraft(c.max_retry_attempts),
    retry_backoff_ms: numberDraft(c.retry_backoff_ms),
    use_structured_content: c.use_structured_content || false,
    read_only_tools: c.read_only_tools || [],
  };
}

// Throws on invalid JSON in the Args / Headers fields, or a retry number that
// is not one, so the caller can block the save and surface it — parsing to
// an empty value and saving anyway silently discarded whatever the user typed.
export function pack(form: McpFormData): Partial<McpServer> {
  const base: Partial<McpServer> = { name: form.name, enabled: form.enabled };
  const config: McpServerConfig = { endpoint: form.endpoint };
  if (form.auth_mode === 'header' || !form.auth_mode) {
    const headers = parseHeadersText(form.headers);
    if (headers) config.headers = headers;
  }
  if (form.auth_mode === 'oauth') {
    config.auth_mode = 'oauth';
    if (form.oauth_client_id) config.oauth_client_id = form.oauth_client_id;
    if (form.oauth_client_secret) config.oauth_client_secret = form.oauth_client_secret;
    if (form.oauth_scopes) config.oauth_scopes = form.oauth_scopes;
  } else if (form.auth_mode === 'header') {
    config.auth_mode = 'header';
  }
  const retries = parseWholeNumber(form.max_retry_attempts, 'Max retry attempts');
  const backoff = parseWholeNumber(form.retry_backoff_ms, 'Retry backoff');
  if (retries) config.max_retry_attempts = retries;
  if (backoff) config.retry_backoff_ms = backoff;
  if (form.use_structured_content) config.use_structured_content = true;
  if (form.read_only_tools.length > 0) config.read_only_tools = form.read_only_tools;
  return { ...base, config };
}

// readOnlyHinted picks the tools a server marks read-only, in listing order:
// the list a person adopts with one click, as a statement of trust in the
// server's hints — the hints alone admit nothing (invariant 89).
export function readOnlyHinted(tools: McpTool[]): string[] {
  return tools.filter(t => t.read_only_hint).map(t => t.original_name);
}

export function McpForm({ initial, onSave, onCancel, onDelete, saving, onClearAuth }: McpFormProps) {
  const [form, setForm] = useState<McpFormData>(flatten(initial || {}));
  const [authCleared, setAuthCleared] = useState(false);
  const [clearing, setClearing] = useState(false);
  const set = (k: keyof McpFormData, v: string | boolean | number | string[]) => setForm(prev => ({ ...prev, [k]: v }));
  // The server's tools, asked for while it is connected: the list to pick
  // the plan-mode allowance from. Disconnected, the saved names show as they are.
  const connected = initial?.status === 'connected';
  const [tools, setTools] = useState<McpTool[] | null>(null);
  useEffect(() => {
    if (!connected || !initial?.id) return;
    let stale = false;
    (api.mcpServers.tools(initial.id) as Promise<McpTool[]>).then(t => { if (!stale) setTools(t); }).catch(() => { if (!stale) setTools([]); });
    return () => { stale = true; };
  }, [connected, initial?.id]);
  const toggleReadOnly = (name: string, on: boolean) =>
    set('read_only_tools', on ? [...form.read_only_tools.filter(n => n !== name), name] : form.read_only_tools.filter(n => n !== name));
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
      {fc('Max retry attempts', <TextInput block type="number" min={-1} value={form.max_retry_attempts} placeholder="0" onChange={e => set('max_retry_attempts', e.target.value)} />, '0 = no retries, -1 = retry indefinitely; a retried call_tool may run twice')}
      {Number(form.max_retry_attempts.trim() || 0) !== 0 && fc('Retry backoff (ms)', <TextInput block type="number" min={0} value={form.retry_backoff_ms} placeholder="0" onChange={e => set('retry_backoff_ms', e.target.value)} />, 'Base delay for exponential backoff (0 = default 1000ms)')}
      <ToggleRow label="Use structured content" checked={form.use_structured_content} onChange={v => set('use_structured_content', v)}
        description="Use a tool result's structuredContent field exclusively (for servers that only populate it)" />
      {/* Plan mode admits an MCP tool only by name (invariant 89): the names
          are picked here, per tool, with the server's own hints one click
          away as a choice a person makes, never a default. */}
      {(initial?.id) && (
        <div className="form-group">
          <div className="form-group-title">Allowed while planning</div>
          {connected && tools === null && <div className="FormControl-caption">Asking the server for its tools…</div>}
          {connected && tools && tools.length === 0 && <div className="FormControl-caption">The server lists no tools.</div>}
          {connected && tools && tools.length > 0 && (
            <>
              <div className="form-checkbox-group">
                {tools.map(t => (
                  <ToggleRow key={t.original_name} label={t.original_name} checked={form.read_only_tools.includes(t.original_name)}
                    onChange={v => toggleReadOnly(t.original_name, v)}
                    description={(t.read_only_hint ? 'Marked read-only by the server. ' : '') + (t.description || '')} />
                ))}
              </div>
              <Button size="small" onClick={() => set('read_only_tools', readOnlyHinted(tools))} disabled={readOnlyHinted(tools).length === 0}>
                Select the ones the server marks read-only
              </Button>
            </>
          )}
          {!connected && (
            <div className="FormControl-caption">
              {form.read_only_tools.length > 0 ? 'Saved: ' + form.read_only_tools.join(', ') + ' — connect the server to change the list.' : 'Connect the server to pick which of its tools plan mode may call.'}
            </div>
          )}
        </div>
      )}
      <ToggleRow label="Enabled" checked={form.enabled} onChange={v => set('enabled', v)} />
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

// The words behind the dot, for the tooltip and the accessibility tree.
const STATUS_TEXT: Record<McpStatus, string> = {
  connected: 'connected',
  connecting: 'connecting',
  authorizing: 'authorizing',
  needs_auth: 'needs authorization',
  disconnected: 'disconnected',
  disabled: 'disabled',
};

// The action button each status offers; connected and disabled offer none.
// connecting is disabled (a concurrent connect would just error with
// "already in progress"), but authorizing stays CLICKABLE: the wait is on the
// user finishing a popup they may have closed, and re-clicking supersedes the
// stale attempt server-side (OAuthCoordinator.supersedeInflight) and opens a
// fresh popup — otherwise a closed popup pins the row for the full 5-minute
// pending timeout.
const STATUS_ACTION: Partial<Record<McpStatus, { label: string; inProgress?: boolean }>> = {
  disconnected: { label: 'Connect' },
  needs_auth: { label: 'Authorize' },
  connecting: { label: 'Connecting…', inProgress: true },
  authorizing: { label: 'Authorizing… (retry)' },
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
const OAUTH_POPUP = 'width=520,height=640,popup=yes';

// A live authorization's URL, kept behind the row's "Open sign-in page" link
// while the flow is in flight (a blocked popup leaves that the only way in).
type AuthorizeLink = { url: string; at: number };

// AUTH_PENDING are the statuses under which a connect is expected to ask for
// authorization, and the row shows its link.
const AUTH_PENDING = new Set<McpStatus>(['needs_auth', 'authorizing']);

export function McpServerPanel() {
  const { me } = useMe();
  const isAdmin = me?.role === 'admin';
  const rowEditable = (s: McpServer) => canEditRow(isAdmin, me?.id, s);
  const { items: servers, loading, error, reload, adding, editing, startAdd, startEdit, cancel, save, saving, remove } = useCrud<McpServer, Partial<McpServer>>(api.mcpServers, 'mcp-servers');
  const [query, setQuery] = useState('');
  const scopeFilter = useScopeFilter();
  const rows = filterRows(servers, { mine: !!scopeFilter?.mine, meId: me?.id, query }, s => `${s.name} ${(s.config && s.config.endpoint) || ''}`);
  const transfer = useTransfer({ kindLabel: 'MCP servers', setOwner: api.mcpServers.setOwner, onDone: reload });
  const confirmDialog = useConfirm();
  // busy covers only the POST /connect round-trip; every longer-lived state
  // (connecting, authorizing) is reported by the backend via status.
  const [busy, setBusy] = useState<Record<string | number, boolean>>({});
  const [graceUntil, setGraceUntil] = useState(0);
  const bumpGrace = useCallback(() => setGraceUntil(Date.now() + MUTATION_GRACE_MS), []);
  const [authorizeLink, setAuthorizeLink] = useState<Record<string | number, AuthorizeLink>>({});
  // A link outlives its flow only through the grace window after the click
  // (the poll has not flipped the row to authorizing yet); a row seen in any
  // other status past it — connected, timed out back to needs_auth — drops it.
  useEffect(() => {
    setAuthorizeLink(prev => {
      const now = Date.now();
      const next: Record<string | number, AuthorizeLink> = {};
      for (const [id, link] of Object.entries(prev)) {
        const row = servers.find(s => String(s.id) === id);
        if (row && (row.status === 'authorizing' || now - link.at < MUTATION_GRACE_MS)) next[id] = link;
      }
      return Object.keys(next).length === Object.keys(prev).length ? prev : next;
    });
  }, [servers]);

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

  const handleConnect = async (s: McpServer) => {
    const id = s.id;
    setBusy(prev => ({ ...prev, [id]: true }));
    // A popup opened after the await is not the click's any more and gets
    // blocked, so a row expected to ask for authorization opens one now and
    // points it once the URL is known; a plain reconnect opens none.
    let popup = AUTH_PENDING.has(s.status) ? window.open('', 'mcp_oauth', OAUTH_POPUP) : null;
    try {
      const res = await api.mcpServers.connect(id) as { status?: string; authorize_url?: string } | null;
      if (res && res.status === 'authorization_required' && res.authorize_url) {
        setAuthorizeLink(prev => ({ ...prev, [id]: { url: res.authorize_url!, at: Date.now() } }));
        if (popup) popup.location.href = res.authorize_url;
        else popup = window.open(res.authorize_url, 'mcp_oauth', OAUTH_POPUP);
      } else {
        popup?.close();
      }
    } catch (e: unknown) {
      popup?.close();
      toast.error((e as Error).message || 'Connect failed');
    }
    setBusy(prev => ({ ...prev, [id]: false }));
    bumpGrace();
    reload();
  };
  const handleClearAuth = async (id: string | number): Promise<boolean> => {
    const ok = await confirmDialog({
      title: 'Clear the saved authorization?',
      content: 'The server disconnects and its OAuth token is deleted; the next connect asks for authorization again.',
      confirmButtonContent: 'Clear auth',
      confirmButtonType: 'danger',
    });
    if (!ok) return false;
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
      <CrudPanel title="MCP servers" onAdd={startAdd} onCancel={cancel} form={form} loading={loading} error={error} onRetry={reload} isEmpty={rows.length === 0}
        search={{ value: query, onChange: setQuery, placeholder: 'Search MCP servers' }}
        {...listEmpty({ noun: 'MCP servers', total: servers.length, query, mine: !!scopeFilter?.mine, hint: 'An MCP server lends its tools to the agents that select it.' })}
        onDelete={editing && canDeleteRow(isAdmin, me?.id, editing)
          ? async () => { if (await remove(editing.id, editing.name)) cancel(); } : null}>
        {rows.map(s => {
          const action = STATUS_ACTION[s.status];
          const editable = rowEditable(s);
          return (
            <ResourceRow key={s.id}
              status={<span className="form-status-dot" role="img" title={STATUS_TEXT[s.status] || s.status} aria-label={STATUS_TEXT[s.status] || s.status}
                style={{ background: STATUS_DOT[s.status] || 'var(--fgColor-muted)' }} />}
              title={s.name}
              badges={<>
                <ScopeBadge row={s} meId={me?.id} />
                {s.config && s.config.auth_mode === 'oauth' && <Label variant={BADGE.type}>OAuth</Label>}
              </>}
              sub={(s.config && s.config.endpoint) || ''}
              actions={<>
                {action && editable && (
                  <Button
                    onClick={() => handleConnect(s)}
                    disabled={action.inProgress || busy[s.id]}
                    size="small"
                    className="mcp-connect-btn"
                  >{busy[s.id] ? '…' : action.label}</Button>
                )}
                {editable && AUTH_PENDING.has(s.status) && authorizeLink[s.id] && (
                  <Link href={authorizeLink[s.id].url} target="_blank" rel="noopener">Open sign-in page</Link>
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
                  onTransfer={isAdmin ? () => transfer.start(s) : undefined}
                  scope={{ row: s, setScope: api.mcpServers.setScope, canPromote: isAdmin, canDemote: canDemoteRow(isAdmin, me?.id, s), onDone: reload }} />
              </>}
            />
          );
        })}
      </CrudPanel>
      {transfer.dialog}
    </ReadOnlyContext>
  );
}

export default McpServerPanel;
