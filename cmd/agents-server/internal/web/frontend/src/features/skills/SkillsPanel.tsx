import { useState } from 'react';
import { Button, TextInput, Textarea, Label, Stack, PageHeader, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { toast } from '@/lib/toast';
import { type Skill, groupBySource } from '@/lib/skills';
import { BADGE } from '@/lib/badges';
import { useReadOnly } from '@/lib/access';

const NEW_SKILL_TEMPLATE = `---
name: my-skill
description: What this skill is for — the model activates it when a task matches.
---

# My skill

Step-by-step instructions the model follows once activated.
`;

// What one import did, as the server reports it (skipped entries carry reasons).
interface ImportResult {
  repo: string;
  created?: string[];
  updated?: string[];
  unchanged?: string[];
  skipped?: string[];
  truncated?: boolean;
}

function importSummary(r: ImportResult): string {
  const parts = [
    r.created?.length && `${r.created.length} added`,
    r.updated?.length && `${r.updated.length} updated`,
    r.unchanged?.length && `${r.unchanged.length} unchanged`,
    r.skipped?.length && `${r.skipped.length} skipped`,
  ].filter(Boolean);
  return parts.length ? parts.join(', ') : 'nothing imported';
}

// The editor is one Textarea: the document is the skill, its frontmatter is
// the metadata — no separate name/description fields to drift.
function SkillEditor({ initial, onSave, onCancel, onDelete, saving }: {
  initial: string;
  onSave: (content: string) => void;
  onCancel: () => void;
  onDelete?: () => void;
  saving: boolean;
}) {
  const [content, setContent] = useState(initial);
  return (
    <Stack gap="condensed">
      <Textarea
        value={content}
        onChange={e => setContent(e.target.value)}
        rows={18}
        block
        resize="vertical"
        style={{ fontFamily: 'var(--fontStack-monospace)', fontSize: 12 }}
      />
      <Stack direction="horizontal" gap="condensed">
        <Button variant="primary" size="small" disabled={saving} onClick={() => onSave(content)}>Save</Button>
        <Button size="small" onClick={onCancel}>Cancel</Button>
        {onDelete && <Button variant="danger" size="small" onClick={onDelete} style={{ marginLeft: 'auto' }}>Delete</Button>}
      </Stack>
    </Stack>
  );
}

type Mode =
  | { kind: 'import' }
  | { kind: 'new' }
  | { kind: 'edit'; skill: Skill };

export function SkillsPanel() {
  const readOnly = useReadOnly();
  const confirmDialog = useConfirm();
  const { data: skills, loading, error, reload } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>);
  const [mode, setMode] = useState<Mode | null>(null);
  const [importUrl, setImportUrl] = useState('');
  const [busy, setBusy] = useState(false);
  const [syncing, setSyncing] = useState('');

  const runImport = async (url: string) => {
    const result = (await api.skills.import(url)) as ImportResult;
    for (const s of result.skipped || []) toast.error('Skipped ' + s);
    if (result.truncated) toast.error('Repository listing was truncated — files past the cut were not seen');
    toast.info(importSummary(result));
  };

  const handleImport = async () => {
    const url = importUrl.trim();
    if (!url) return;
    setBusy(true);
    try {
      await runImport(url);
      setImportUrl('');
      setMode(null);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Import failed');
    } finally {
      setBusy(false);
    }
  };

  const handleSync = async (repo: string) => {
    setSyncing(repo);
    try {
      await runImport(repo);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Sync failed');
    } finally {
      setSyncing('');
    }
  };

  const handleCreate = async (content: string) => {
    setBusy(true);
    try {
      await api.skills.create({ content });
      setMode(null);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Save failed');
    } finally {
      setBusy(false);
    }
  };

  const handleUpdate = async (id: string, content: string) => {
    setBusy(true);
    try {
      await api.skills.update(id, { content });
      setMode(null);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Save failed');
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async (sk: Skill) => {
    const ok = await confirmDialog({
      title: `Delete “${sk.name}”?`,
      content: 'Agents that select this skill simply stop advertising it. This cannot be undone.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    });
    if (!ok) return;
    try {
      await api.skills.delete(sk.id);
      setMode(null);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Delete failed');
    }
  };

  const openEditor = async (sk: Skill) => {
    try {
      const full = (await api.skills.get(sk.id)) as Skill;
      setMode({ kind: 'edit', skill: full });
    } catch (e) {
      toast.error((e as Error).message || 'Load failed');
    }
  };

  const grouped = groupBySource(skills || []);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Skills</PageHeader.Title>
        </PageHeader.TitleArea>
        {!mode && !readOnly && (
          <PageHeader.Actions>
            <Button onClick={() => setMode({ kind: 'import' })} size="small">Import</Button>
            <Button onClick={() => setMode({ kind: 'new' })} variant="primary" size="small">+ New</Button>
          </PageHeader.Actions>
        )}
      </PageHeader>

      {mode?.kind === 'import' && (
        <Stack gap="normal" direction="horizontal" align="center">
          <TextInput block
            placeholder="https://github.com/owner/repo — or a raw SKILL.md URL"
            value={importUrl}
            onChange={e => setImportUrl(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void handleImport(); }}
            autoFocus
          />
          <Button variant="primary" size="small" disabled={busy || !importUrl.trim()} onClick={handleImport}>
            {busy ? 'Importing…' : 'Import'}
          </Button>
          <Button size="small" onClick={() => { setMode(null); setImportUrl(''); }}>Cancel</Button>
        </Stack>
      )}

      {mode?.kind === 'new' && (
        <SkillEditor initial={NEW_SKILL_TEMPLATE} saving={busy}
          onSave={handleCreate} onCancel={() => setMode(null)} />
      )}
      {mode?.kind === 'edit' && (
        <SkillEditor initial={mode.skill.content || ''} saving={busy}
          onSave={content => handleUpdate(mode.skill.id, content)}
          onCancel={() => setMode(null)}
          onDelete={readOnly ? undefined : () => handleDelete(mode.skill)} />
      )}

      {loading && <div className="resource-row-sub">Loading…</div>}
      {error && <div className="resource-row-sub">{error}</div>}
      {!loading && !error && grouped.length === 0 && mode === null && (
        <Blankslate>
          <Blankslate.Heading>No skills installed</Blankslate.Heading>
          <Blankslate.Description>
            Create a SKILL.md in the workbench, or import every skill from a GitHub repository.
          </Blankslate.Description>
        </Blankslate>
      )}

      {mode === null && grouped.map(group => (
        <div key={group.repo || 'local'} className="Box">
          <div className="Box-row" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span className="resource-row-title">{group.label}</span>
            <span className="resource-row-sub">{group.skills.length} skill{group.skills.length === 1 ? '' : 's'}</span>
            {group.repo !== '' && !readOnly && (
              <Button size="small" style={{ marginLeft: 'auto' }} disabled={syncing === group.repo}
                onClick={() => handleSync(group.repo)}>
                {syncing === group.repo ? 'Syncing…' : 'Sync'}
              </Button>
            )}
          </div>
          {group.skills.map(sk => (
            <div key={sk.id} className="Box-row" style={{ cursor: 'pointer' }} onClick={() => void openEditor(sk)}>
              <div className="resource-row-main">
                <div className="resource-row-head">
                  <span className="resource-row-title">{sk.name}</span>
                  {sk.detached && <Label variant={BADGE.type}>edited</Label>}
                </div>
                <div className="resource-row-sub">{sk.description}</div>
              </div>
            </div>
          ))}
        </div>
      ))}
    </Stack>
  );
}

export default SkillsPanel;
