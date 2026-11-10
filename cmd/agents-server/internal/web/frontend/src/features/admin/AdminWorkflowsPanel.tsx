import { useCallback, useMemo } from 'react';
import { ActionList, Label, useConfirm } from '@primer/react';
import type { Column } from '@primer/react/experimental';
import { GearIcon } from '@primer/octicons-react';
import { RowMenu, actionsColumn } from '@/components/ListTable';
import { useTransfer } from '@/components/TransferDialog';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useApi } from '@/lib/hooks';
import { AdminListPage } from '@/features/admin/AdminListPage';
import { OwnerCell, ownerLabel } from '@/features/admin/OwnerCell';
import { useOwnerLabels } from '@/lib/owners';
import { scopeFlipPrompt } from '@/lib/scopeFlipPrompt';
import { toast } from '@/lib/toast';

// One workflow, flattened to what this table shows.
interface WorkflowRow {
  id: string;
  name: string;
  detail: string;
  scope: string;
  owner_id?: string;
}

const listWorkflows = async (): Promise<WorkflowRow[]> => ((await api.workflows.list()) ?? []).map(w => ({
  id: w.id || '', name: w.name || '', detail: `${(w.steps || []).length} steps`,
  scope: w.scope || 'private', owner_id: w.owner_id,
}));

// AdminWorkflowsPanel: every member's workflow definitions — author, published
// or not, and the admin's two acts (publish/unpublish, transfer). The other
// scoped entities list every member's rows in their own settings tab
// (invariant 61); workflows are authored in the hub, so this is their tab.
export function AdminWorkflowsPanel() {
  const { data: rows, error, reload } = useApi<WorkflowRow[]>(listWorkflows, [], 'admin:workflows');
  const { ownerOf, labelFor } = useOwnerLabels();
  const confirm = useConfirm();
  const transfer = useTransfer({ kindLabel: 'Workflows', setOwner: api.workflows.setOwner, onDone: reload });
  const startTransfer = transfer.start;

  const flip = useCallback(async (row: WorkflowRow) => {
    const target = row.scope === 'global' ? 'private' : 'global';
    if (!(await confirm(scopeFlipPrompt(row.name, target, { id: row.owner_id, label: row.owner_id ? labelFor(row.owner_id) : undefined })))) return;
    try {
      await api.workflows.setScope(row.id, target);
      reload();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to change the scope.');
    }
  }, [confirm, reload, labelFor]);

  const columns = useMemo<Column<WorkflowRow>[]>(() => [
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
    actionsColumn<WorkflowRow>(r => (
      <RowMenu label={`Actions for ${r.name}`}>
        <ActionList.Item onSelect={() => { void flip(r); }}>{r.scope === 'global' ? 'Unpublish' : 'Publish'}</ActionList.Item>
        <ActionList.Item onSelect={() => startTransfer(r)}>Transfer…</ActionList.Item>
      </RowMenu>
    )),
  ], [flip, startTransfer, ownerOf, labelFor]);

  return (
    <AdminListPage
      titleId="admin-workflows-title"
      title="Workflows"
      description="Every member's workflow definitions. Publishing shares one with the team; steps are re-validated on a transfer."
      what="workflows"
      rows={rows}
      error={error}
      onRetry={reload}
      columns={columns}
      match={(r, q) => `${r.name} ${ownerLabel(labelFor, r.owner_id)} ${r.detail}`.toLowerCase().includes(q)}
      empty={{ icon: GearIcon, heading: 'No workflows', text: 'Rows list here as members create them.' }}
    >
      {transfer.dialog}
    </AdminListPage>
  );
}

export default AdminWorkflowsPanel;
