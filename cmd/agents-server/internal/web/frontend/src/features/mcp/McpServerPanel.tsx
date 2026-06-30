import React, { useState, useEffect, useCallback, useRef } from 'react';
import { Button, TextInput, Label, Select, Checkbox, FormControl, Stack, PageHeader, ToggleSwitch } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { toast } from '@/lib/toast';

const TRANSPORTS = ['stdio', 'streamable_http'] as const;
const AUTH_MODES = [
  { value: '', label: 'None' },
  { value: 'header', label: 'Static headers' },
  { value: 'oauth', label: 'OAuth' },
] as const;

interface McpServerConfig {
  command?: string;
  args?: string[];
  endpoint?: string;
  headers?: Record<string, string>;
  auth_mode?: string;
  oauth_client_id?: string;
  oauth_client_secret?: string;
  oauth_scopes?: string;
}

interface McpServer {
  id: string | number;
  name: string;
  transport_type: string;
  enabled: boolean;
  connected?: boolean;
  auth_state?: string;
  config?: McpServerConfig;
}

interface McpFormData {
  name: string;
  transport_type: string;
  enabled: boolean;
  command: string;
  args: string;
  endpoint: string;
  headers: string;
  auth_mode: string;
  oauth_client_id: string;
  oauth_client_secret: string;
  oauth_scopes: string;
}

interface McpFormProps {
  initial?: McpServer;
  onSave: (data: Partial<McpServer>) => void;
  onCancel?: () => void;
  onDelete?: () => void;
}

function flatten(s: Partial<McpServer>): McpFormData {
  const c = s.config || {};
  return {
    name: s.name || '', transport_type: s.transport_type || 'stdio', enabled: s.enabled !== false,
    command: c.command || '',
    args: Array.isArray(c.args) ? JSON.stringify(c.args) : '',
    endpoint: c.endpoint || '',
    headers: c.headers ? JSON.stringify(c.headers) : '',
    auth_mode: c.auth_mode || '',
    oauth_client_id: c.oauth_client_id || '',
    oauth_client_secret: c.oauth_client_secret || '',
    oauth_scopes: c.oauth_scopes || '',
  };
}

function pack(form: McpFormData): Partial<McpServer> {
  const base: Partial<McpServer> = { name: form.name, transport_type: form.transport_type, enabled: form.enabled };
  let config: McpServerConfig;
  if (form.transport_type === 'stdio') {
    let args: string[] = [];
    try { args = form.args ? JSON.parse(form.args) : []; } catch (e) { args = []; }
    config = { command: form.command, args };
  } else {
    config = { endpoint: form.endpoint };
    if (form.auth_mode === 'header' || !form.auth_mode) {
      let headers: Record<string, string> | null = null;
      try { headers = form.headers ? JSON.parse(form.headers) : null; } catch (e) { headers = null; }
      if (headers && typeof headers === 'object' && Object.keys(headers).length > 0) config.headers = headers;
    }
    if (form.auth_mode === 'oauth') {
      config.auth_mode = 'oauth';
      if (form.oauth_client_id) config.oauth_client_id = form.oauth_client_id;
      if (form.oauth_client_secret) config.oauth_client_secret = form.oauth_client_secret;
      if (form.oauth_scopes) config.oauth_scopes = form.oauth_scopes;
    } else if (form.auth_mode === 'header') {
      config.auth_mode = 'header';
    }
  }
  return { ...base, config };
}

function McpForm({ initial, onSave, onCancel, onDelete }: McpFormProps) {
  const [form, setForm] = useState<McpFormData>(flatten(initial || {}));
  const set = (k: keyof McpFormData, v: string | boolean) => setForm(prev => ({ ...prev, [k]: v }));
  const isStdio = form.transport_type === 'stdio';
  const isOAuth = form.auth_mode === 'oauth';
  const isHeader = form.auth_mode === 'header';

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput value={form.name} onChange={e => set('name', e.target.value)} />)}
      {fc('Transport',
        <Select value={form.transport_type} onChange={e => set('transport_type', e.target.value)}>
          {TRANSPORTS.map(t => <Select.Option key={t} value={t}>{t}</Select.Option>)}
        </Select>,
      )}
      {isStdio && fc('Command', <TextInput block value={form.command} onChange={e => set('command', e.target.value)} placeholder="npx -y @modelcontextprotocol/server-filesystem" />)}
      {isStdio && <JsonField label="Args (JSON array)" value={form.args} onChange={v => set('args', v)} placeholder='["/path/to/dir"]' />}
      {!isStdio && fc('Endpoint', <TextInput block value={form.endpoint} onChange={e => set('endpoint', e.target.value)} placeholder="http://localhost:3000/mcp" />)}
      {!isStdio && fc('Authentication',
        <Select value={form.auth_mode} onChange={e => set('auth_mode', e.target.value)}>
          {AUTH_MODES.map(m => <Select.Option key={m.value} value={m.value}>{m.label}</Select.Option>)}
        </Select>,
      )}
      {!isStdio && isHeader && <JsonField label="Headers (JSON object)" value={form.headers} onChange={v => set('headers', v)} placeholder='{"Authorization": "Bearer <token>"}' caption="Sent with every request, e.g. an auth or API-key header. Leave empty for none." />}
      {!isStdio && isOAuth && fc('Client ID',
        <TextInput value={form.oauth_client_id} onChange={e => set('oauth_client_id', e.target.value)} placeholder="Leave empty for dynamic registration" monospace />,
        'Pre-registered OAuth client ID. Leave empty to use dynamic client registration (DCR).',
      )}
      {!isStdio && isOAuth && form.oauth_client_id && fc('Client secret',
        <TextInput value={form.oauth_client_secret} onChange={e => set('oauth_client_secret', e.target.value)} type="password" monospace />,
      )}
      {!isStdio && isOAuth && fc('Scopes',
        <TextInput value={form.oauth_scopes} onChange={e => set('oauth_scopes', e.target.value)} placeholder="read write" monospace />,
        'Space-separated OAuth scopes to request.',
      )}
      <FormControl>
        <Checkbox checked={form.enabled} onChange={e => set('enabled', e.target.checked)} />
        <FormControl.Label>Enabled</FormControl.Label>
      </FormControl>
      <div className="form-actions">
        <Button onClick={() => onSave(pack(form))} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

function statusDot(s: McpServer): string {
  if (s.connected) return 'var(--fgColor-success)';
  if (s.auth_state === 'unauthorized') return 'var(--fgColor-attention, var(--fgColor-muted))';
  return 'var(--fgColor-muted)';
}

function connectLabel(s: McpServer, isConnecting: boolean): string {
  if (isConnecting) return '...';
  if (s.auth_state === 'unauthorized') return 'Authorize';
  if (s.auth_state === 'authorizing') return 'Authorizing...';
  return 'Connect';
}

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

export function McpServerPanel() {
  const { items: servers, reload, adding, editing, startAdd, startEdit, cancel, save, remove } = useCrud<McpServer, Partial<McpServer>>(api.mcpServers);
  const [connecting, setConnecting] = useState<Record<string | number, boolean>>({});
  const [authorizing, setAuthorizing] = useState<Record<string | number, boolean>>({});
  const pollRef = useRef<Record<string | number, { interval: ReturnType<typeof setInterval>; timeout: ReturnType<typeof setTimeout> }>>({});

  const stopPoll = useCallback((id: string | number) => {
    const entry = pollRef.current[id];
    if (entry) {
      clearInterval(entry.interval);
      clearTimeout(entry.timeout);
      delete pollRef.current[id];
    }
    setAuthorizing(prev => {
      if (!prev[id]) return prev;
      return { ...prev, [id]: false };
    });
  }, []);

  const stopAllPolls = useCallback(() => {
    for (const id of Object.keys(pollRef.current)) stopPoll(id);
  }, [stopPoll]);

  useEffect(() => stopAllPolls, [stopAllPolls]);

  useEffect(() => {
    const handler = (event: MessageEvent) => {
      if (event.data && event.data.type === 'mcp-oauth-done') {
        reload();
      }
    };
    window.addEventListener('message', handler);
    return () => window.removeEventListener('message', handler);
  }, [reload]);

  const handleConnect = async (id: string | number) => {
    setConnecting(prev => ({ ...prev, [id]: true }));
    try {
      const res = await api.mcpServers.connect(id) as { status?: string; authorize_url?: string } | null;
      if (res && res.status === 'authorization_required' && res.authorize_url) {
        stopPoll(id);
        setAuthorizing(prev => ({ ...prev, [id]: true }));
        window.open(res.authorize_url, 'mcp_oauth', 'width=520,height=640,popup=yes');
        const interval = setInterval(async () => {
          try {
            const srv = await api.mcpServers.get(id) as McpServer | null;
            if (srv && srv.connected) {
              stopPoll(id);
              reload();
            }
          } catch (_) { /* ignore transient errors */ }
        }, 2000);
        const timeout = setTimeout(() => { stopPoll(id); reload(); }, 5 * 60 * 1000);
        pollRef.current[id] = { interval, timeout };
      }
    } catch (e: unknown) {
      toast.error((e as Error).message || 'Connect failed');
    }
    setConnecting(prev => ({ ...prev, [id]: false }));
    reload();
  };
  const handleDisconnect = async (id: string | number) => {
    try {
      await api.mcpServers.disconnect(id);
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const handleToggleEnabled = async (s: McpServer) => {
    try {
      await api.mcpServers.update(s.id, { ...s, enabled: !s.enabled });
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>MCP Servers</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>
      {adding && <McpForm onSave={save} onCancel={cancel} />}
      {editing && <McpForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} />}
      {!adding && !editing && <div className="Box">
        {servers.map(s => (
          <div key={s.id} className="Box-row">
            <div className="resource-row-main">
              <div className="form-status">
                <span className="form-status-dot" style={{ background: statusDot(s) }} />
                <span className="resource-row-title">{s.name}</span>
                {s.config && s.config.auth_mode === 'oauth' && <Label variant="secondary">OAuth</Label>}
              </div>
              <div className="resource-row-sub" style={{ marginLeft: '16px' }}>
                {s.transport_type + (s.config && s.config.command ? ': ' + s.config.command : '') + (s.config && s.config.endpoint ? ': ' + s.config.endpoint : '')}
              </div>
            </div>
            <div className="resource-row-actions">
              <EnabledToggle server={s} onToggle={handleToggleEnabled} />
              {!s.connected
                ? <Button
                    onClick={() => handleConnect(s.id)}
                    disabled={connecting[s.id] || authorizing[s.id]}
                    size="small"
                    style={{ color: 'var(--fgColor-success)', minWidth: 90, textAlign: 'center' }}
                  >{connectLabel(s, connecting[s.id] || authorizing[s.id])}</Button>
                : <Button onClick={() => handleDisconnect(s.id)} size="small" variant="invisible" style={{ minWidth: 90, textAlign: 'center' }}>Disconnect</Button>
              }
              <Button onClick={() => startEdit(s)} size="small" variant="invisible">Edit</Button>
            </div>
          </div>
        ))}
        {servers.length === 0 && (
          <Blankslate>
            <Blankslate.Description>No MCP servers configured.</Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default McpServerPanel;
