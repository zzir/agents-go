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
}

const GROUP_TITLES: Record<string, string> = {
  network: 'Network',
  prompt: 'Prompt',
  tracing: 'Tracing',
  logging: 'Logging',
  limits: 'Limits',
  storage: 'Attachment storage',
};

export function SettingsPanel() {
  const readOnly = useReadOnly();
  const { data: defs } = useApi<SettingDef[]>(() => api.settings.defs() as Promise<SettingDef[]>);
  const { data: settings, reload } = useApi<Setting[]>(() => api.settings.list() as Promise<Setting[]>);
  const [saving, setSaving] = useState<Record<string, boolean>>({});

  const getValue = useCallback((key: string): string => {
    return settings?.find(s => s.key === key)?.value ?? '';
  }, [settings]);

  const handleSave = async (key: string, value: string): Promise<boolean> => {
    setSaving(prev => ({ ...prev, [key]: true }));
    try {
      await api.settings.set(key, value);
      reload();
      return true;
    } catch (e) {
      // A rejected value now carries the server's reason ("… must be at most 32").
      toast.error((e as Error).message);
      return false;
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
          {g.name === 'storage' ? (
            <StorageForm defs={g.defs} getValue={getValue} onSaved={reload} />
          ) : (
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
          )}
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
  onSave: (value: string) => Promise<boolean>;
}

function SettingRow({ def, value, saving, onSave }: SettingRowProps) {
  const [draft, setDraft] = useState(value);
  const changed = draft !== value;
  useEffect(() => { setDraft(value); }, [value]);

  // A segmented control reads as applied on click, so for bools it is: the
  // click stores the value at once and reverts on failure. The draft-and-Save
  // step exists only for the typed kinds.
  const instant = def.kind === 'bool';
  const setOrSave = instant
    ? (v: string) => { setDraft(v); void onSave(v).then(ok => { if (!ok) setDraft(value); }); }
    : setDraft;

  // The default belongs in the caption, not in a placeholder the operator has
  // to guess at: it is what the server actually applies when the box is empty.
  const caption = [def.description, def.default ? `Default: ${def.default}.` : null]
    .filter(Boolean).join(' ');

  return (
    <FormControl>
      <FormControl.Label>{def.label}</FormControl.Label>
      {caption && <FormControl.Caption>{caption}</FormControl.Caption>}
      <SettingInput def={def} draft={draft} setDraft={setOrSave} />
      {changed && !instant && (
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
          rows={8}
          placeholder={def.placeholder}
          block
          style={{ fontFamily: 'var(--fontStack-monospace)' }}
        />
      );
    case 'bool': {
      // Two states: every bool has a registered default, and the server reads
      // unset as that default (Reader.Bool) — so the control is On/Off with
      // the default side holding until a value is stored.
      const options: [string, string][] = [['true', 'On'], ['false', 'Off']];
      const selected = draft === '' ? def.default : draft;
      return (
        // onChange makes the control controlled. Without it Primer keeps the
        // selection in internal state seeded on first render — before the
        // settings fetch resolves — and ignores `selected` from then on.
        <SegmentedControl aria-label={def.label} size="small" onChange={i => setDraft(options[i][0])}>
          {options.map(([v, text]) => (
            <SegmentedControl.Button key={v} selected={selected === v}>
              {text}
            </SegmentedControl.Button>
          ))}
        </SegmentedControl>
      );
    }
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

// The storage-section keys mapped to the group endpoint's field names. The
// section's values are only valid together (changing the bucket re-probes
// against the same public base), so this is a FORM — one Save, one Test, one
// Clear — not click-to-store rows; the server refuses per-key writes of
// these keys.
const STORAGE_FIELDS: Record<string, string> = {
  s3_endpoint: 'endpoint',
  s3_region: 'region',
  s3_bucket: 'bucket',
  s3_access_key_id: 'access_key_id',
  s3_secret_access_key: 'secret_access_key',
  s3_public_base_url: 'public_base_url',
  s3_path_style: 'path_style',
};

function StorageForm({ defs, getValue, onSaved }: { defs: SettingDef[]; getValue: (key: string) => string; onSaved: () => void }) {
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<'save' | 'test' | 'clear' | null>(null);
  const stored = defs.map(d => getValue(d.key)).join('\u0000');
  useEffect(() => {
    const next: Record<string, string> = {};
    for (const d of defs) next[d.key] = getValue(d.key);
    setDraft(next);
    // Re-seed when the stored values change (initial load, save, clear).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stored]);

  const body = (clear: boolean): Record<string, unknown> => {
    if (clear) return {};
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const field = STORAGE_FIELDS[d.key] ?? d.key;
      out[field] = d.kind === 'bool' ? (draft[d.key] || d.default) === 'true' : (draft[d.key] ?? '').trim();
    }
    return out;
  };

  const run = async (kind: 'save' | 'test' | 'clear') => {
    setBusy(kind);
    try {
      if (kind === 'test') {
        await api.attachments.storageTest(body(false));
        toast.success('Bucket verified — upload, anonymous read and delete all passed');
      } else {
        await api.attachments.storageSave(body(kind === 'clear'));
        toast.success(kind === 'clear' ? 'Attachment storage cleared — image input is off' : 'Attachment storage saved');
        onSaved();
      }
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(null);
    }
  };

  return (
    <Stack gap="spacious">
      {defs.map(def => (
        <FormControl key={def.key}>
          <FormControl.Label>{def.label}</FormControl.Label>
          {def.description && <FormControl.Caption>{def.description}</FormControl.Caption>}
          <SettingInput def={def} draft={draft[def.key] ?? ''} setDraft={v => setDraft(prev => ({ ...prev, [def.key]: v }))} />
        </FormControl>
      ))}
      <Stack direction="horizontal" gap="condensed">
        <Button variant="primary" size="small" onClick={() => run('save')} disabled={busy !== null}>
          {busy === 'save' ? 'Saving…' : 'Save'}
        </Button>
        <Button size="small" onClick={() => run('test')} disabled={busy !== null}>
          {busy === 'test' ? 'Testing…' : 'Test'}
        </Button>
        <Button variant="danger" size="small" onClick={() => run('clear')} disabled={busy !== null}>
          {busy === 'clear' ? 'Clearing…' : 'Clear'}
        </Button>
      </Stack>
    </Stack>
  );
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
