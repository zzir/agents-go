import { useState } from 'react';
import { ActionList, Button, TextInput, Textarea, Label, Stack, PageHeader, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { RowMenu } from '@/components/ListTable';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { toast } from '@/lib/toast';
import { type Skill, type SkillGroup, groupSkills } from '@/lib/skills';
import { BADGE } from '@/lib/badges';
import { canDeleteRow, canDemoteRow, canEditRow } from '@/lib/access';
import { useMe } from '@/lib/me';
import { useOwnerLabels } from '@/lib/owners';
import { OwnerBadge, ScopeBadge } from '@/components/CrudPanel';

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

  const runImport = async (url: string, ownerId?: string) => {
    const result = (await api.skills.import(url, ownerId)) as ImportResult;
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

  // A sync names the GROUP it refreshes, not just the repo: the same
  // repository can be two groups (one published, one somebody's private
  // copy), and a sync must land in the one whose row was clicked (§5.31).
  const handleSync = async (group: SkillGroup) => {
    setSyncing(prev => new Set(prev).add(group.key));
    try {
      await runImport(group.repo, group.ownerId || undefined);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Sync failed');
    } finally {
      setSyncing(prev => { const next = new Set(prev); next.delete(group.key); return next; });
    }
  };

  // An imported repo flips as ONE group, server-side and all-or-nothing —
  // a repo's skills publish together, so the group is never half-published
  // (spec §5.29). A workbench-authored skill flips on its own row.
  const setGroupScope = async (group: SkillGroup, scope: 'global' | 'private') => {
    if (group.repo !== '') {
      try {
        await api.skills.setRepoScope(group.repo, scope, group.ownerId || undefined);
      } catch (e) {
        toast.error((e as Error).message || 'Scope change failed');
      }
      reload();
      return;
    }
    // The Local bucket is not a group: each row flips on its own, so one
    // refusal (a name taken in the target scope) must not abort the rest.
    const targets = group.skills.filter(sk => sk.scope !== scope);
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

  // Groups are (repo, owner): a listing carrying other people's rows — the
  // admin's, or a published repo — names whose each group is, and a group
  // flip's blast radius is exactly what its heading says.
  const { labelFor } = useOwnerLabels();
  const grouped = groupSkills(skills || [], me?.id, labelFor);

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

      {loading && !error && <div className="resource-row-sub">Loading…</div>}
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
        // is offered only when every row is the caller's to update. Publishing
        // a group is the admin's; unpublishing is theirs or its author's.
        const canSync = group.repo !== '' && group.skills.every(skillEditable);
        const owner = { scope: group.scope, owner_id: group.ownerId };
        const isRepo = group.repo !== '';
        // A Local bucket flips per row, so it offers both directions while it
        // holds rows to move; a repo group has one scope and one direction.
        const canPublish = isAdmin && (isRepo ? group.scope !== 'global' : group.skills.some(sk => sk.scope !== 'global'));
        const canUnpublish = isRepo
          ? group.scope === 'global' && canDemoteRow(isAdmin, me?.id, owner)
          : group.skills.some(sk => sk.scope === 'global' && canDemoteRow(isAdmin, me?.id, sk));
        const groupItems = (canSync ? 1 : 0) + (canPublish ? 1 : 0) + (canUnpublish ? 1 : 0);
        return (
        <div key={group.key} className="Box">
          <div className="Box-row" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span className="resource-row-title">{group.label}</span>
            {/* Scope and author sit on the GROUP: a repo publishes as one. */}
            {group.scope && <ScopeBadge row={owner} meId={me?.id} />}
            <OwnerBadge row={owner} meId={me?.id} labelFor={labelFor} />
            <span className="resource-row-sub">{group.skills.length} skill{group.skills.length === 1 ? '' : 's'}</span>
            {syncing.has(group.key) && <span className="resource-row-sub">Syncing…</span>}
            {groupItems > 0 && (
              <div style={{ marginLeft: 'auto' }}>
                <RowMenu label={`Actions for ${group.label}`}>
                  {canSync && <ActionList.Item disabled={syncing.has(group.key)} onSelect={() => void handleSync(group)}>Sync</ActionList.Item>}
                  {canPublish && <ActionList.Item onSelect={() => void setGroupScope(group, 'global')}>{isRepo ? 'Make global' : 'Make all global'}</ActionList.Item>}
                  {canUnpublish && <ActionList.Item onSelect={() => void setGroupScope(group, 'private')}>{isRepo ? 'Make private' : 'Make all private'}</ActionList.Item>}
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
                  {/* A Local bucket can hold both scopes; a repo group's scope
                      is on its heading, so the row stays quiet. */}
                  {!group.scope && <ScopeBadge row={sk} meId={me?.id} />}
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
