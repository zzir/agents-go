import { useState, useCallback, useEffect, type ChangeEvent } from 'react';
import { Button, TextInput, Textarea, FormControl, Stack, PageHeader, Select } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { fc } from '@/lib/form';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { toast } from '@/lib/toast';

interface Setting { key: string; value: string }
interface ProviderRoute { id: string; prefix: string; provider_id: string }

// The endpoints a route can point at; managed under Providers.
interface ProviderRef { id: string; name: string }

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
  { key: 'openai_api_key', label: 'OpenAI API key (fallback)', placeholder: 'sk-... (******** keeps the stored key)', description: 'Used by agents and fallback-model entries on the OpenAI provider that have no API key of their own.' },
  { key: 'anthropic_api_key', label: 'Anthropic API key (fallback)', placeholder: 'sk-ant-... (******** keeps the stored key)', description: 'Used by agents and fallback-model entries on the Anthropic provider that have no API key of their own.' },
  { key: 'brave_api_key', label: 'Brave Search API key', placeholder: 'BSA-xxxxxxxx', description: 'When set, a brave_search tool is injected into all agents. Get a key at brave.com/search/api.' },
  { key: 'trace_retention_days', label: 'Trace retention (days)', placeholder: 'e.g. 30 — empty disables pruning', description: 'Trace events older than this many days are pruned daily. Leave empty (or 0) to keep everything.' },
  { key: 'trace_include_sensitive_data', label: 'Trace sensitive data', placeholder: 'true (default) or false', description: 'Set to false to keep prompts, outputs and tool arguments out of stored traces — spans then carry only timing and usage metadata (the trace panel\'s Replay has nothing to seed from). Applies to new runs.' },
  { key: 'trace_span_data_kb', label: 'Stored span payload (KB)', placeholder: 'e.g. 8192 — empty uses the default', description: 'How much of a span\'s model request and response is stored. Past it the payload is replaced with a marker and a Replay of that call has nothing to seed from — raise it if you replay large turns. Live updates to the browser are capped separately at 256KB; what they drop is still in the trace. Applies to new runs.' },
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

interface RouteDraft { prefix: string; provider_id: string }
const EMPTY_ROUTE: RouteDraft = { prefix: '', provider_id: '' };

function RouteForm({ initial, onSave, onCancel, onDelete, providers }: {
  initial?: RouteDraft;
  onSave: (d: RouteDraft) => void;
  onCancel: () => void;
  onDelete?: () => void;
  providers: ProviderRef[] | null;
}) {
  const [draft, setDraft] = useState<RouteDraft>(initial || EMPTY_ROUTE);

  return (
    <Stack gap="normal">
      {fc('Prefix', (
        <TextInput
          value={draft.prefix}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(d => ({ ...d, prefix: e.target.value }))}
          placeholder="e.g. groq or anthropic"
          block
        />
      ))}
      {fc('Provider', (
        <Select value={draft.provider_id} onChange={e => setDraft(d => ({ ...d, provider_id: e.target.value }))} block>
          <Select.Option value="">Select a provider…</Select.Option>
          {(providers || []).map(p => <Select.Option key={p.id} value={p.id}>{p.name}</Select.Option>)}
        </Select>
      ), 'The endpoint this prefix routes to — its credential lives there')}
      <div className="form-actions">
        <Button onClick={() => { if (draft.prefix && draft.provider_id) onSave(draft); }} variant="primary" size="small">Save</Button>
        <Button onClick={onCancel} size="small">Cancel</Button>
        {onDelete && <Button onClick={onDelete} variant="danger" size="small" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

function ProviderRoutesSection() {
  const { items: routes, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<ProviderRoute, RouteDraft>(api.providerRoutes);
  const { data: providers } = useApi<ProviderRef[]>(() => api.providers.list() as Promise<ProviderRef[]>);
  const providerName = (id: string) => (providers || []).find(p => p.id === id)?.name || id.slice(0, 8);

  return (
    <div className="form-group">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as="h3">Provider Routes</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
        <PageHeader.Description>Route model names by prefix (e.g. &quot;groq/llama-3&quot; &rarr; prefix &quot;groq&quot;). The agent&apos;s own provider is the fallback.</PageHeader.Description>
      </PageHeader>
      {adding && <RouteForm onSave={save} onCancel={cancel} providers={providers} />}
      {editing && <RouteForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} providers={providers} />}
      {!adding && !editing && <div className="Box">
        {routes.map(r => (
          <div key={r.id} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-head">
                <span className="resource-row-title">{r.prefix}/</span>
                <span className="resource-row-sub">&rarr; {providerName(r.provider_id)}</span>
              </div>
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
