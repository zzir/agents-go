import { useState, useCallback, useEffect, type ChangeEvent } from 'react';
import { Button, TextInput, Textarea, FormControl, Stack, PageHeader, SegmentedControl, Label } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { useReadOnly } from '@/lib/access';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { toast } from '@/lib/toast';

// A stored row. `unknown` marks a key the server's registry no longer defines
// — listed so it can be deleted, since nothing else would ever show it.
interface Setting { key: string; value: string; unknown?: boolean }

// One entry of the server's settings registry (GET /setting-defs). The panel
// renders from this, so adding a global setting is a Go change alone.
interface SettingDef {
  key: string;
  kind: 'string' | 'text' | 'secret' | 'int' | 'bool';
  group: string;
  label: string;
  description?: string;
  placeholder?: string;
  default?: string;
  min?: number;
  max?: number;
}

// The start-up configuration (GET /server): shown, never edited — it comes
// from the command line, not this table.
interface ServerInfo {
  version: string;
  workspace: string;
  max_tasks: number;
}

const GROUP_TITLES: Record<string, string> = {
  network: 'Network',
  prompt: 'Prompt',
  credentials: 'Credentials',
  tracing: 'Tracing',
  logging: 'Logging',
  limits: 'Limits',
};

export function SettingsPanel() {
  const readOnly = useReadOnly();
  const { data: defs } = useApi<SettingDef[]>(() => api.settings.defs() as Promise<SettingDef[]>);
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
      // A rejected value now carries the server's reason ("… must be at most 32").
      toast.error((e as Error).message);
    } finally {
      setSaving(prev => ({ ...prev, [key]: false }));
    }
  };

  const handleDelete = async (key: string) => {
    try {
      await api.settings.delete(key);
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const unknown = (settings || []).filter(s => s.unknown);
  // Group headers come from the def order the server serves, not a table here.
  const groups: { name: string; defs: SettingDef[] }[] = [];
  for (const def of defs || []) {
    const last = groups[groups.length - 1];
    if (last && last.name === def.group) last.defs.push(def);
    else groups.push({ name: def.group, defs: [def] });
  }

  return (
    <Stack gap="spacious">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>General</PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>Network, prompt, and integration settings that apply to all agents.</PageHeader.Description>
      </PageHeader>
      {/* A member sees the values and can change none: one fieldset over
          the rows disables every input and the Save each would show. */}
      <fieldset disabled={readOnly} className="readonly-form settings-form">
      {groups.map(g => (
        <div key={g.name} className="form-group">
          <PageHeader>
            <PageHeader.TitleArea>
              <PageHeader.Title as="h3">{GROUP_TITLES[g.name] || g.name}</PageHeader.Title>
            </PageHeader.TitleArea>
          </PageHeader>
          <Stack gap="spacious">
            {g.defs.map(def => (
              <SettingRow
                key={def.key}
                def={def}
                value={getValue(def.key)}
                saving={saving[def.key]}
                onSave={v => handleSave(def.key, v)}
              />
            ))}
          </Stack>
        </div>
      ))}
      </fieldset>
      {unknown.length > 0 && <UnknownSection rows={unknown} onDelete={readOnly ? null : handleDelete} />}
      <ServerSection />
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

  // The default belongs in the caption, not in a placeholder the operator has
  // to guess at: it is what the server actually applies when the box is empty.
  const caption = [def.description, def.default ? `Default: ${def.default}.` : null]
    .filter(Boolean).join(' ');

  return (
    <FormControl>
      <FormControl.Label>{def.label}</FormControl.Label>
      {caption && <FormControl.Caption>{caption}</FormControl.Caption>}
      <SettingInput def={def} draft={draft} setDraft={setDraft} />
      {changed && (
        <Button onClick={() => onSave(draft)} disabled={saving} variant="primary" size="small">
          {saving ? 'Saving…' : 'Save'}
        </Button>
      )}
    </FormControl>
  );
}

function SettingInput({ def, draft, setDraft }: { def: SettingDef; draft: string; setDraft: (v: string) => void }) {
  switch (def.kind) {
    case 'text':
      return (
        <Textarea
          value={draft}
          onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setDraft(e.target.value)}
          rows={3}
          placeholder={def.placeholder}
          block
          style={{ fontFamily: 'var(--fontStack-monospace)' }}
        />
      );
    case 'bool':
      // Three states, not a checkbox: an empty value is its own answer —
      // "whatever the server decides" — and for trace_include_sensitive_data
      // that is what lets the SDK read its environment variable. A checkbox
      // would silently convert unset into an explicit choice on first save.
      return (
        <SegmentedControl aria-label={def.label} size="small">
          {([['', def.default ? `Default (${def.default})` : 'Default'], ['true', 'On'], ['false', 'Off']] as const).map(([v, text]) => (
            <SegmentedControl.Button key={v || 'default'} selected={draft === v} onClick={() => setDraft(v)}>
              {text}
            </SegmentedControl.Button>
          ))}
        </SegmentedControl>
      );
    case 'int':
      return (
        <TextInput
          type="number"
          value={draft}
          // A whole-number setting never goes negative, and `min: 0` is
          // omitted from the JSON — so the floor is 0 unless a def raises it.
          // Without this the spinner would offer values the server rejects.
          min={def.min ?? 0}
          max={def.max}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          placeholder={def.placeholder || def.default}
          block
        />
      );
    case 'secret':
      return (
        <SecretInput
          value={draft}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          placeholder="******** keeps the stored value"
          block
        />
      );
    default:
      return (
        <TextInput
          value={draft}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
          placeholder={def.placeholder}
          block
        />
      );
  }
}

// Rows the registry does not define: written before writes were validated, or
// left behind by a removed feature. Shown rather than hidden, because a value
// nobody can see is a value nobody can clear.
function UnknownSection({ rows, onDelete }: { rows: Setting[]; onDelete: ((key: string) => void) | null }) {
  return (
    <div className="form-group">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as="h3">Unrecognized</PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>Stored keys this server does not define. Nothing reads them.</PageHeader.Description>
      </PageHeader>
      <div className="Box">
        {rows.map(r => (
          <div key={r.key} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-head">
                <span className="resource-row-title">{r.key}</span>
                <Label variant="attention">unknown</Label>
              </div>
              <div className="resource-row-sub">{r.value}</div>
            </div>
            {onDelete && (
              <div className="resource-row-actions">
                <Button onClick={() => onDelete(r.key)} size="small" variant="danger">Delete</Button>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

// The flags in force. Not editable — but a rule you cannot see is one you can
// only meet as an unexplained refusal.
function ServerSection() {
  const { data: info } = useApi<ServerInfo>(() => api.server() as Promise<ServerInfo>);
  if (!info) return null;
  const rows: [string, string][] = [
    ['Version', info.version],
    ['Workspace', info.workspace],
    ['Background tasks per session', String(info.max_tasks)],
  ];
  return (
    <div className="form-group">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as="h3">Server</PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>Set on the command line at start-up. Restart to change.</PageHeader.Description>
      </PageHeader>
      <div className="Box">
        {rows.map(([label, value]) => (
          <div key={label} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-head">
                <span className="resource-row-title">{label}</span>
              </div>
              <div className="resource-row-sub" style={{ fontFamily: 'var(--fontStack-monospace)' }}>{value}</div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

export default SettingsPanel;
