import { useCallback, useMemo, useState } from 'react';
import { ActionList, Label, PageHeader, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { GearIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { TransferDialog } from '@/components/TransferDialog';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useApi } from '@/lib/hooks';
import { useMe } from '@/lib/me';
import { OwnerCell, ownerLabel } from '@/features/admin/OwnerCell';
import { useLoadError } from '@/features/admin/useLoadError';
import { LOCAL_USER_ID, useOwnerLabels } from '@/lib/owners';
import { toast } from '@/lib/toast';

// One row, flattened to what this table shows.
interface ConfigRow {
  id: string;
  name: string;
  detail: string;
  scope: string;
  owner_id?: string;
}

// The scoped entities (spec §5.29). Four have a settings panel that lists
// every member's rows itself (invariant 61); only workflows are managed here.
export type ScopedEntity = 'agents' | 'providers' | 'mcp-servers' | 'skills' | 'workflows';

interface EntityKind {
  key: ScopedEntity;
  label: string;
  // One line under the title: what this tab manages, and what it does not.
  blurb: string;
  list: () => Promise<ConfigRow[]>;
  setScope: (id: string, scope: 'global' | 'private') => Promise<null>;
  setOwner: (id: string, userId: string) => Promise<null>;
}

const WORKFLOWS: EntityKind = {
  key: 'workflows', label: 'Workflows',
  blurb: "Every member's workflow definitions. Publishing shares one with the team; steps are re-validated on a transfer.",
  list: async () => ((await api.workflows.list()) ?? []).map(w => ({
    id: w.id || '', name: w.name || '', detail: `${(w.steps || []).length} steps`,
    scope: w.scope || 'private', owner_id: w.owner_id,
  })),
  setScope: api.workflows.setScope, setOwner: api.workflows.setOwner,
};

// ScopedRowsPanel: one entity's rows across every member — who authored each,
// whether it is published, and the two management acts an admin has over it
// (publish/unpublish, and transfer to another account). Editing stays where
// authorship is: the entity's own settings panel.
export function ScopedRowsPanel({ kind }: { kind: EntityKind }) {
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
      renderCell: r => <span className="list-clip" title={r.name}>{r.name}</span>,
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
        <ActionList.Item onSelect={() => { void flip(r); }}>{r.scope === 'global' ? 'Unpublish' : 'Publish'}</ActionList.Item>
        <ActionList.Item onSelect={() => setTransferring(r)}>Transfer…</ActionList.Item>
      </RowMenu>
    )),
  ], [flip, ownerOf, labelFor]);

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

// Workflows have no personal settings panel (the hub is where they are
// authored), so the management view is their whole settings tab.
export const AdminWorkflows = () => <ScopedRowsPanel kind={WORKFLOWS} />;
