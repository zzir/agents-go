import { useState, useCallback, useEffect, type ChangeEvent } from 'react';
import { Button, TextInput, Textarea, FormControl, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { fc } from '@/lib/form';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { toast } from '@/lib/toast';

interface Setting { key: string; value: string }
interface ProviderRoute { id: string; prefix: string; api_key: string; base_url: string }

interface SettingDef {
  key: string;
  label: string;
  placeholder: string;
  description?: string;
  multiline?: boolean;
}

const DEFAULT_KEYS: SettingDef[] = [
  { key: 'proxy_url', label: 'Proxy URL', placeholder: 'http://127.0.0.1:7890 or socks5://127.0.0.1:1080', description: 'All outbound API and MCP HTTP requests will be routed through this proxy.' },
  { key: 'system_prompt', label: 'System prompt', placeholder: 'Optional instructions prepended to all agents', multiline: true },
  { key: 'brave_api_key', label: 'Brave Search API key', placeholder: 'BSA-xxxxxxxx', description: 'When set, a brave_search tool is injected into all agents. Get a key at brave.com/search/api.' },
  { key: 'trace_retention_days', label: 'Trace retention (days)', placeholder: 'e.g. 30 — empty disables pruning', description: 'Trace events older than this many days are pruned daily. Leave empty (or 0) to keep everything.' },
];

export function SettingsPanel() {
  const { data: settings, reload } = useApi<Setting[]>(() => api.settings.list() as Promise<Setting[]>);
  const [saving, setSaving] = useState<Record<string, boolean>>({});

  const getValue = useCallback((key: string): string => {
    return settings?.find(s => s.key === key)?.value ?? '';
  }, [settings]);

  const handleSave = async (key: string, value: string) => {
    setSaving(prev => ({ ...prev, [key]: true }));
    try {
      await api.settings.set(key, value);
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setSaving(prev => ({ ...prev, [key]: false }));
    }
  };

  return (
    <Stack gap="spacious">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>General</PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>Network, prompt, and integration settings that apply to all agents.</PageHeader.Description>
      </PageHeader>
      <Stack gap="spacious">
        {DEFAULT_KEYS.map(def => (
          <SettingRow
            key={def.key}
            def={def}
            value={getValue(def.key)}
            saving={saving[def.key]}
            onSave={v => handleSave(def.key, v)}
          />
        ))}
      </Stack>
      <ProviderRoutesSection />
    </Stack>
  );
}

interface SettingRowProps {
  def: SettingDef;
  value: string;
  saving?: boolean;
  onSave: (value: string) => void;
}

function SettingRow({ def, value, saving, onSave }: SettingRowProps) {
  const [draft, setDraft] = useState(value);
  const changed = draft !== value;
  useEffect(() => { setDraft(value); }, [value]);

  return (
    <FormControl>
      <FormControl.Label>{def.label}</FormControl.Label>
      {def.description && <FormControl.Caption>{def.description}</FormControl.Caption>}
      {def.multiline ? (
        <Textarea
          value={draft}
          onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setDraft(e.target.value)}
          rows={3}
          placeholder={def.placeholder}
          block
          style={{ fontFamily: 'var(--fontStack-monospace)' }}
        />
      ) : (
        <TextInput
          value={draft}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          placeholder={def.placeholder}
          block
        />
      )}
      {changed && (
        <Button onClick={() => onSave(draft)} disabled={saving} variant="primary" size="small">
          {saving ? 'Saving…' : 'Save'}
        </Button>
      )}
    </FormControl>
  );
}

interface RouteDraft { prefix: string; api_key: string; base_url: string }
const EMPTY_ROUTE: RouteDraft = { prefix: '', api_key: '', base_url: '' };

function RouteForm({ initial, onSave, onCancel, onDelete }: {
  initial?: RouteDraft;
  onSave: (d: RouteDraft) => void;
  onCancel: () => void;
  onDelete?: () => void;
}) {
  const [draft, setDraft] = useState<RouteDraft>(initial || EMPTY_ROUTE);

  return (
    <Stack gap="normal">
      {fc('Prefix', (
        <TextInput
          value={draft.prefix}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(d => ({ ...d, prefix: e.target.value }))}
          placeholder="e.g. groq"
          block
        />
      ))}
      {fc('API Key', (
        <TextInput
          value={draft.api_key}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(d => ({ ...d, api_key: e.target.value }))}
          placeholder="API Key (******** keeps the stored key)"
          type="password"
          block
        />
      ))}
      {fc('Base URL', (
        <TextInput
          value={draft.base_url}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(d => ({ ...d, base_url: e.target.value }))}
          placeholder="https://api.groq.com/openai/v1"
          block
        />
      ))}
      <div className="form-actions">
        <Button onClick={() => { if (draft.prefix) onSave(draft); }} variant="primary" size="small">Save</Button>
        <Button onClick={onCancel} size="small">Cancel</Button>
        {onDelete && <Button onClick={onDelete} variant="danger" size="small" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

function ProviderRoutesSection() {
  const { items: routes, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<ProviderRoute, RouteDraft>(api.providerRoutes);

  return (
    <div className="form-group">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as="h3">Provider Routes</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
        <PageHeader.Description>Route model names by prefix (e.g. &quot;groq/llama-3&quot; &rarr; prefix &quot;groq&quot;). The agent&apos;s own provider is the fallback.</PageHeader.Description>
      </PageHeader>
      {adding && <RouteForm onSave={save} onCancel={cancel} />}
      {editing && <RouteForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} />}
      {!adding && !editing && <div className="Box">
        {routes.map(r => (
          <div key={r.id} className="Box-row">
            <div className="resource-row-main">
              <span className="resource-row-title">{r.prefix}/</span>
              {r.base_url && <span className="resource-row-meta">{r.base_url}</span>}
            </div>
            <div className="resource-row-actions">
              <Button onClick={() => startEdit(r)} size="small" variant="invisible">Edit</Button>
            </div>
          </div>
        ))}
        {routes.length === 0 && (
          <Blankslate>
            <Blankslate.Description>No provider routes configured.</Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </div>
  );
}

export default SettingsPanel;
