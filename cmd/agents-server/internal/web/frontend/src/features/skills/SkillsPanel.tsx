import { useState } from 'react';
import { ActionList, Button, TextInput, Textarea, Label, Stack, PageHeader, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { RowMenu } from '@/components/ListTable';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { toast } from '@/lib/toast';
import { type Skill, groupBySource, splitLocalByOwner } from '@/lib/skills';
import { BADGE } from '@/lib/badges';
import { canDeleteRow, canEditRow } from '@/lib/access';
import { useMe } from '@/lib/me';
import { ScopeBadge } from '@/components/CrudPanel';

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
// the metadata — no separate name/description fields to drift. readOnly is a
// skill the caller may not edit (a member's view of a global one); Delete can
// still show there (an admin may delete what it cannot edit).
function SkillEditor({ initial, onSave, onCancel, onDelete, saving, readOnly }: {
  initial: string;
  onSave: (content: string) => void;
  onCancel: () => void;
  onDelete?: () => void;
  saving: boolean;
  readOnly?: boolean;
}) {
  const [content, setContent] = useState(initial);
  return (
    <Stack gap="condensed">
      <Textarea
        value={content}
        onChange={e => setContent(e.target.value)}
        readOnly={readOnly}
        rows={18}
        block
        resize="vertical"
        style={{ fontFamily: 'var(--fontStack-monospace)', fontSize: 12 }}
      />
      <Stack direction="horizontal" gap="condensed">
        {!readOnly && <Button variant="primary" size="small" disabled={saving} onClick={() => onSave(content)}>Save</Button>}
        <Button size="small" onClick={onCancel}>{readOnly ? 'Back' : 'Cancel'}</Button>
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
  const { me } = useMe();
  const isAdmin = me?.role === 'admin';
  const skillEditable = (sk: Skill) => canEditRow(isAdmin, me?.id, sk);
  const confirmDialog = useConfirm();
  const { data: skills, loading, error, reload } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>);
  const [mode, setMode] = useState<Mode | null>(null);
  const [importUrl, setImportUrl] = useState('');
  const [busy, setBusy] = useState(false);
  const [syncing, setSyncing] = useState<Set<string>>(new Set());

  const runImport = async (url: string) => {
    const result = (await api.skills.import(url)) as ImportResult;
    // One toast, not one per file: a repo with many bad SKILL.md's would
    // otherwise flood the stack.
    const skipped = result.skipped || [];
    if (skipped.length > 0) {
      const head = skipped.slice(0, 3).join('; ');
      toast.error(`Skipped ${skipped.length}: ${head}${skipped.length > 3 ? '; …' : ''}`);
    }
    if (result.truncated) toast.error('Repository listing was truncated — files past the cut were not seen');
    toast.info(importSummary(result));
  };

  const handleImport = async () => {
    const url = importUrl.trim();
    if (!url || busy) return;
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
    setSyncing(prev => new Set(prev).add(repo));
    try {
      await runImport(repo);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Sync failed');
    } finally {
      setSyncing(prev => { const next = new Set(prev); next.delete(repo); return next; });
    }
  };

  // Scope flips per GROUP: an import lands a repo's skills together, so
  // publishing them goes together too. Rows already in the target scope are
  // skipped; per-row failures (a name taken in the target scope) are
  // collected, not fatal to the rest.
  const setGroupScope = async (skillsInGroup: Skill[], scope: 'global' | 'private') => {
    const targets = skillsInGroup.filter(sk => sk.scope !== scope);
    const failed: string[] = [];
    for (const sk of targets) {
      try {
        await api.skills.setScope(sk.id, scope);
      } catch (e) {
        failed.push(`${sk.name}: ${(e as Error).message}`);
      }
    }
    if (failed.length > 0) toast.error(`${failed.length} of ${targets.length} not changed — ` + failed.join('; '));
    reload();
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

  // Admin listings see every user's rows; splitting the Local bucket per
  // owner keeps a group flip's blast radius honest. Emails come from the
  // admin-only users listing; a short id fills in while it loads.
  const { data: users } = useApi<{ id: string; email?: string }[]>(
    () => (isAdmin ? (api.auth.users.list() as Promise<{ id: string; email?: string }[]>) : Promise.resolve([])),
    [isAdmin],
  );
  const labelFor = (ownerId: string) =>
    users?.find(u => u.id === ownerId)?.email || ownerId.slice(0, 8);
  const grouped = isAdmin
    ? splitLocalByOwner(groupBySource(skills || []), me?.id, labelFor)
    : groupBySource(skills || []);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Skills</PageHeader.Title>
        </PageHeader.TitleArea>
        {/* Creating is every member's: a new or imported skill lands private,
            owned by them. */}
        {!mode && (
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
          readOnly={!skillEditable(mode.skill)}
          onSave={content => handleUpdate(mode.skill.id, content)}
          onCancel={() => setMode(null)}
          onDelete={canDeleteRow(isAdmin, me?.id, mode.skill) ? () => handleDelete(mode.skill) : undefined} />
      )}

      {loading && <div className="resource-row-sub">Loading…</div>}
      {error && (
        <Blankslate>
          <Blankslate.Heading>Could not load skills</Blankslate.Heading>
          <Blankslate.Description>{error}</Blankslate.Description>
          <Blankslate.PrimaryAction onClick={() => reload()}>Retry</Blankslate.PrimaryAction>
        </Blankslate>
      )}
      {!loading && !error && grouped.length === 0 && mode === null && (
        <Blankslate>
          <Blankslate.Heading>No skills installed</Blankslate.Heading>
          <Blankslate.Description>
            Create a SKILL.md in the workbench, or import every skill from a GitHub repository.
          </Blankslate.Description>
        </Blankslate>
      )}

      {mode === null && grouped.map(group => {
        // Sync re-imports the repo, updating every row in the group — so it
        // is offered only when every row is the caller's to update. The scope
        // flips are the admin's, per group (a repo's skills publish together).
        const canSync = group.repo !== '' && group.skills.every(skillEditable);
        const hasPrivate = group.skills.some(sk => sk.scope !== 'global');
        const hasGlobal = group.skills.some(sk => sk.scope === 'global');
        const groupItems = (canSync ? 1 : 0) + (isAdmin ? (hasPrivate ? 1 : 0) + (hasGlobal ? 1 : 0) : 0);
        return (
        <div key={group.key || group.repo || 'local'} className="Box">
          <div className="Box-row" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span className="resource-row-title">{group.label}</span>
            <span className="resource-row-sub">{group.skills.length} skill{group.skills.length === 1 ? '' : 's'}</span>
            {syncing.has(group.repo) && group.repo !== '' && <span className="resource-row-sub">Syncing…</span>}
            {groupItems > 0 && (
              <div style={{ marginLeft: 'auto' }}>
                <RowMenu label={`Actions for ${group.label}`}>
                  {canSync && <ActionList.Item disabled={syncing.has(group.repo)} onSelect={() => void handleSync(group.repo)}>Sync</ActionList.Item>}
                  {isAdmin && hasPrivate && <ActionList.Item onSelect={() => void setGroupScope(group.skills, 'global')}>Make all global</ActionList.Item>}
                  {isAdmin && hasGlobal && <ActionList.Item onSelect={() => void setGroupScope(group.skills, 'private')}>Make all private</ActionList.Item>}
                </RowMenu>
              </div>
            )}
          </div>
          {group.skills.map(sk => (
            <div key={sk.id} className="Box-row" style={{ cursor: 'pointer' }} role="button" tabIndex={0}
              onClick={() => void openEditor(sk)}
              onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); void openEditor(sk); } }}>
              <div className="resource-row-main">
                <div className="resource-row-head">
                  <span className="resource-row-title">{sk.name}</span>
                  <ScopeBadge row={sk} meId={me?.id} />
                  {sk.detached && <Label variant={BADGE.type}>edited</Label>}
                </div>
                <div className="resource-row-sub">{sk.description}</div>
              </div>
            </div>
          ))}
        </div>
        );
      })}
    </Stack>
  );
}

export default SkillsPanel;
