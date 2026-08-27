import { useCallback, useEffect, useMemo, useState } from 'react';
import { ActionList, Flash, PageHeader, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { FileDirectoryIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';
import { OwnerName, useOwnerLabels } from '@/lib/owners';

type ProjectRow = Omit<ApiSchemas['store.Project'], 'id'> & { id: string; sandbox: string };

// ProjectsPanel: every owner's working trees and where their files live — the
// operator's map of what exists, and of what a delete destroys. Newest first:
// what an admin watches here is what has just appeared.
export function ProjectsPanel() {
  const [projects, setProjects] = useState<ProjectRow[] | null>(null);
  const [error, setError] = useState('');
  const confirm = useConfirm();
  const { ownerOf, labelFor } = useOwnerLabels();

  const reload = useCallback(() => {
    Promise.all([api.sandboxTargets.list(), api.projects.listAll()])
      .then(([targets, rows]) => {
        const tgName = (id?: string) => (targets ?? []).find(t => t.id === id)?.name || id || '';
        setProjects((rows ?? []).map(p => ({ ...p, id: p.id || '', sandbox: tgName(p.target_id) })));
        setError('');
      })
      .catch(() => setError('Failed to load projects.'));
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const remove = useCallback(async (p: ProjectRow) => {
    if (!(await confirm({
      title: `Delete “${p.name}”?`,
      content: p.storage_hint
        ? `This DESTROYS its working tree — ${p.storage_hint} — and a Docker volume is not in anyone's backup. Refused while sessions are still bound to it.`
        : 'This DESTROYS its working tree, and a Docker volume is not in anyone\'s backup. Refused while sessions are still bound to it.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    }))) return;
    try {
      await api.projects.delete(p.id);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to delete the project.');
    }
  }, [confirm, reload]);

  const columns = useMemo<Column<ProjectRow>[]>(() => [
    { header: 'Project', id: 'name', rowHeader: true, width: 'growCollapse', minWidth: 120, renderCell: p => <span className="list-clip" title={p.name}>{p.name}</span> },
    { header: 'Owner', id: 'owner', width: 'growCollapse', minWidth: 100, maxWidth: 220, renderCell: p => <OwnerName owner={ownerOf(p.owner_id)} fallback={labelFor(p.owner_id)} /> },
    { header: 'Machine', id: 'sandbox', width: 'growCollapse', minWidth: 90, maxWidth: 180, renderCell: p => <span className="list-clip" title={p.sandbox}>{p.sandbox}</span> },
    { header: 'Storage', id: 'storage', width: 'growCollapse', minWidth: 140, renderCell: p => <span className="list-clip" title={p.storage_hint}>{p.storage_hint}</span> },
    { header: 'Sessions', id: 'sessions', width: 'auto', renderCell: p => <span className="list-nowrap">{p.session_count ?? 0}</span> },
    // The list is newest first, so the date it sorts on is on the row.
    { header: 'Created', id: 'created', width: 'auto', renderCell: p => <span className="list-nowrap">{p.created_at ? shortTime(p.created_at) : ''}</span> },
    actionsColumn<ProjectRow>(p => (
      <RowMenu label={`Actions for ${p.name}`}>
        <ActionList.Item variant="danger" onSelect={() => { void remove(p); }}>Delete</ActionList.Item>
      </RowMenu>
    )),
  ], [remove, ownerOf, labelFor]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="admin-projects-title">Projects</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          Every owner&apos;s working trees, and where each one&apos;s files live.
          Deleting a row leaves its directory or volume in place — reclaiming
          the space is done on the daemon.
        </PageHeader.Description>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      <ListTable
        labelledBy="admin-projects-title"
        rows={projects ?? []}
        columns={columns}
        loading={projects === null}
        search={{ placeholder: 'Search projects', match: (p, q) => `${p.name || ''} ${labelFor(p.owner_id)} ${p.sandbox} ${p.storage_hint || ''}`.toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><FileDirectoryIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>No projects</Blankslate.Heading>
            <Blankslate.Description>Working trees list here as members create them.</Blankslate.Description>
          </Blankslate>
        )}
      />
    </Stack>
  );
}

export default ProjectsPanel;
