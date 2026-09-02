import { useCallback, useMemo, useState } from 'react';
import { ActionList, Dialog, FormControl, Label, PageHeader, Select, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { GearIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { AgentAvatar } from '@/components/AgentAvatar';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useApi } from '@/lib/hooks';
import { useMe } from '@/lib/me';
import { LOCAL_USER_ID } from '@/features/admin/MembersPanel';
import { OwnerCell, ownerLabel } from '@/features/admin/OwnerCell';
import { useLoadError } from '@/features/admin/useLoadError';
import { useOwnerLabels } from '@/lib/owners';
import { qualifiedName, repoLabel, type Skill } from '@/lib/skills';
import { toast } from '@/lib/toast';

// One row of any scoped entity, flattened to what this panel shows.
interface ConfigRow {
  id: string;
  name: string;
  detail: string;
  scope: string;
  owner_id?: string;
  // Agents only: the built-in avatar path, rendered before the name.
  avatar?: string;
}

export type ScopedEntity = 'agents' | 'providers' | 'mcp-servers' | 'skills' | 'workflows';

// The five entities members compose runs from (spec §5.29). Each knows how to
// list itself, how to name a row, and which endpoints move it.
interface EntityKind {
  key: ScopedEntity;
  label: string;
  // One line under the title: what this tab manages, and what it does not.
  blurb: string;
  // Rows are agents: render an avatar before each name.
  avatars?: boolean;
  list: () => Promise<ConfigRow[]>;
  setScope: (id: string, scope: 'global' | 'private') => Promise<null>;
  setOwner: (id: string, userId: string) => Promise<null>;
  // Skills flip with their repo group, not one row at a time.
  scopePerRow: (row: ConfigRow) => boolean;
}

type Listed = { id?: string; name?: string; scope?: string; owner_id?: string; avatar?: string };

const base = (r: Listed, detail: string): ConfigRow => ({
  id: r.id || '', name: r.name || '', detail, scope: r.scope || 'private',
  owner_id: r.owner_id, avatar: r.avatar,
});

const ENTITIES: Record<ScopedEntity, EntityKind> = {
  agents: {
    key: 'agents', label: 'Agents', avatars: true,
    blurb: "Every member's agents. Publishing shares one with the team; transferring hands it to another account.",
    list: async () => ((await api.agents.list()) ?? []).map(a => base(a, a.model || '')),
    setScope: api.agents.setScope, setOwner: api.agents.setOwner, scopePerRow: () => true,
  },
  providers: {
    key: 'providers', label: 'Providers',
    blurb: "Every endpoint and its credential. A transfer moves the key and is refused while other owners' agents reference it.",
    list: async () => ((await api.providers.list()) ?? []).map(p => base(p, p.base_url || p.type || '')),
    setScope: api.providers.setScope, setOwner: api.providers.setOwner, scopePerRow: () => true,
  },
  'mcp-servers': {
    key: 'mcp-servers', label: 'MCP servers',
    blurb: "Every member's MCP servers. Publishing shares one with the team; its authorization stays with its author.",
    list: async () => ((await api.mcpServers.list()) ?? []).map(s => base(s, s.status || '')),
    setScope: api.mcpServers.setScope, setOwner: api.mcpServers.setOwner, scopePerRow: () => true,
  },
  skills: {
    key: 'skills', label: 'Skills',
    blurb: 'Every stored SKILL.md. An imported one moves with its whole repository; only workbench-authored skills act alone.',
    list: async () => (((await api.skills.list()) as Skill[]) ?? []).map(sk => ({
      ...base(sk, repoLabel(sk.source_repo || '') || 'workbench'), name: qualifiedName(sk),
    })),
    setScope: api.skills.setScope, setOwner: api.skills.setOwner,
    // An imported skill's scope belongs to its repo group — the Skills panel
    // flips it there; here only workbench-authored rows offer it.
    scopePerRow: row => row.detail === 'workbench',
  },
  workflows: {
    key: 'workflows', label: 'Workflows',
    blurb: "Every member's workflow definitions. Publishing shares one with the team; steps are re-validated on a transfer.",
    list: async () => ((await api.workflows.list()) ?? []).map(w => base(w, `${(w.steps || []).length} steps`)),
    setScope: api.workflows.setScope, setOwner: api.workflows.setOwner, scopePerRow: () => true,
  },
};

// ScopedRowsPanel: one entity's rows across every member — who authored each,
// whether it is published, and the two management acts an admin has over it
// (publish/unpublish, and transfer to another account). Editing stays where
// authorship is: the entity's own settings panel.
export function ScopedRowsPanel({ entity }: { entity: ScopedEntity }) {
  const kind = ENTITIES[entity];
  const { data: rows, error, reload } = useApi<ConfigRow[]>(kind.list, [kind], `admin:${kind.key}`);
  useLoadError(error, kind.label.toLowerCase());
  const [transferring, setTransferring] = useState<ConfigRow | null>(null);
  const { me } = useMe();
  const { users, ownerOf, labelFor } = useOwnerLabels();
  const confirm = useConfirm();

  const flip = useCallback(async (row: ConfigRow) => {
    const target = row.scope === 'global' ? 'private' : 'global';
    if (!(await confirm({
      title: target === 'global' ? `Publish “${row.name}”?` : `Unpublish “${row.name}”?`,
      content: target === 'global'
        ? 'Every member will see it. Its author keeps it and can still edit it.'
        : row.owner_id
          ? `It returns to ${labelFor(row.owner_id)} alone; members using it lose access.`
          : 'Members using it lose access. It has no author to return to — transfer it first if someone should keep it.',
      confirmButtonContent: target === 'global' ? 'Publish' : 'Unpublish',
    }))) return;
    try {
      await kind.setScope(row.id, target);
      reload();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to change the scope.');
    }
  }, [confirm, kind, reload, labelFor]);

  const columns = useMemo<Column<ConfigRow>[]>(() => [
    {
      header: 'Name', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 140,
      renderCell: r => kind.avatars
        ? <span className="list-clip agent-inline" title={r.name}>
            <AgentAvatar name={r.name} avatar={r.avatar} size={20} />{r.name}
          </span>
        : <span className="list-clip" title={r.name}>{r.name}</span>,
    },
    {
      // A badge needs its own room: 'auto' collapses the column to the header
      // and the label spills over the next one.
      header: 'Visibility', id: 'scope', width: 'growCollapse', minWidth: 88, maxWidth: 110,
      renderCell: r => (r.scope === 'global' ? <Label variant={BADGE.scope}>Global</Label> : <span className="resource-row-sub">Private</span>),
    },
    { header: 'Owner', id: 'owner', width: 'growCollapse', minWidth: 120, maxWidth: 240, renderCell: r => <OwnerCell ownerId={r.owner_id} ownerOf={ownerOf} labelFor={labelFor} /> },
    { header: 'Detail', id: 'detail', width: 'growCollapse', minWidth: 100, maxWidth: 220, renderCell: r => <span className="list-clip" title={r.detail}>{r.detail}</span> },
    actionsColumn<ConfigRow>(r => (
      <RowMenu label={`Actions for ${r.name}`}>
        {kind.scopePerRow(r)
          ? <ActionList.Item onSelect={() => { void flip(r); }}>{r.scope === 'global' ? 'Unpublish' : 'Publish'}</ActionList.Item>
          : <ActionList.Item disabled>Scope follows its repository</ActionList.Item>}
        <ActionList.Item onSelect={() => setTransferring(r)}>Transfer…</ActionList.Item>
      </RowMenu>
    )),
  ], [flip, kind, ownerOf, labelFor]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id={`admin-${kind.key}-title`}>{kind.label}</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>{kind.blurb}</PageHeader.Description>
      </PageHeader>
      <ListTable
        labelledBy={`admin-${kind.key}-title`}
        rows={rows ?? []}
        columns={columns}
        loading={rows === null}
        search={{ placeholder: `Search ${kind.label.toLowerCase()}`, match: (r, q) => `${r.name} ${ownerLabel(labelFor, r.owner_id)} ${r.detail}`.toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><GearIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>No {kind.label.toLowerCase()}</Blankslate.Heading>
            <Blankslate.Description>Rows list here as members create them.</Blankslate.Description>
          </Blankslate>
        )}
      />
      {transferring ? (
        <TransferDialog
          row={transferring}
          kindLabel={kind.label}
          owner={ownerLabel(labelFor, transferring.owner_id)}
          users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== transferring.owner_id)}
          transfer={userId => kind.setOwner(transferring.id, userId)}
          onClose={() => setTransferring(null)}
          onDone={() => { setTransferring(null); reload(); }}
          meId={me?.id}
        />
      ) : null}
    </Stack>
  );
}

// TransferDialog picks the account a scoped row moves to. Scope stays put: a
// published row stays published, under its new author.
function TransferDialog({ row, kindLabel, owner, users, transfer, onClose, onDone, meId }: {
  row: ConfigRow;
  kindLabel: string;
  owner: string;
  users: { id: string; name?: string; email: string }[];
  transfer: (userId: string) => Promise<null>;
  onClose: () => void;
  onDone: () => void;
  meId?: string;
}) {
  const [userId, setUserId] = useState(users[0]?.id || '');
  const [busy, setBusy] = useState(false);

  const run = useCallback(async () => {
    setBusy(true);
    try {
      await transfer(userId);
      onDone();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to transfer.');
      setBusy(false);
    }
  }, [transfer, userId, onDone]);

  return (
    <Dialog
      title={`Transfer ${kindLabel.replace(/s$/, '').toLowerCase()}`}
      onClose={onClose}
      width="medium"
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: 'Transfer', disabled: busy || !userId, onClick: () => { void run(); } },
      ]}
    >
      <Stack gap="normal">
        <FormControl required>
          <FormControl.Label>New owner</FormControl.Label>
          <FormControl.Caption>
            &quot;{row.name}&quot; ({owner}) becomes theirs to edit — any credential on it
            included. It stays {row.scope === 'global' ? 'published to the team' : 'private'}.
            {kindLabel === 'Skills' && row.detail !== 'workbench' ? ' Every skill imported from the same repository moves with it.' : ''}
            {row.owner_id === meId ? ' You will no longer be its author.' : ''}
          </FormControl.Caption>
          <Select block value={userId} onChange={e => setUserId(e.target.value)}>
            {users.length === 0 ? <Select.Option value="">No other account</Select.Option> : null}
            {users.map(u => <Select.Option key={u.id} value={u.id}>{u.name ? `${u.name} (${u.email})` : u.email}</Select.Option>)}
          </Select>
        </FormControl>
      </Stack>
    </Dialog>
  );
}

// Workflows have no personal settings panel (the hub is where they are
// authored), so the management view is their whole settings tab.
export const AdminWorkflows = () => <ScopedRowsPanel entity="workflows" />;
