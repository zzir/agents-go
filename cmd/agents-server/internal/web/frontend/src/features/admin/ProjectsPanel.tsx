import { useCallback, useEffect, useMemo, useState } from 'react';
import { ActionList, Flash, PageHeader, Stack, useConfirm } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { FileDirectoryIcon } from '@primer/octicons-react';
import { ListTable, RowMenu, actionsColumn } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';
import { OwnerName, useOwnerLabels } from '@/lib/owners';

type ProjectRow = Omit<ApiSchemas['store.Project'], 'id'> & { id: string; sandbox: string; sandboxType: string };

// ProjectsPanel: every owner's working trees and where their files live — the
// operator's map of what exists, and of what a delete destroys. Newest first:
// what an admin watches here is what has just appeared.
//
// The compute actions are the admin's too (stop, and rebuild where the backend
// has one): they are strictly less than the delete already offered here.
// Reading a tree — preview, export — is not, and is nowhere on this page.
export function ProjectsPanel() {
  const [projects, setProjects] = useState<ProjectRow[] | null>(null);
  const [error, setError] = useState('');
  const confirm = useConfirm();
  const { ownerOf, labelFor } = useOwnerLabels();

  const reload = useCallback(() => {
    Promise.all([api.sandboxes.list(), api.projects.listAll()])
      .then(([sandboxes, rows]) => {
        const of = (id?: string) => (sandboxes ?? []).find(sb => sb.id === id);
        setProjects((rows ?? []).map(p => ({
          ...p,
          id: p.id || '',
          sandbox: of(p.sandbox_id)?.name || p.sandbox_id || '',
          sandboxType: of(p.sandbox_id)?.type || '',
        })));
        setError('');
      })
      .catch(() => setError('Failed to load projects.'));
  }, []);

  // Stop and rebuild answer 204, or an upstream error worth reading — a
  // rebuild refused because the sandbox IS the storage says so in its message.
  const act = useCallback(async (p: ProjectRow, what: 'stop' | 'rebuild') => {
    try {
      if (what === 'stop') await api.projects.sandboxStop(p.id);
      else await api.projects.rebuildContainer(p.id);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : `Failed to ${what} the sandbox.`);
    }
  }, []);
  useEffect(() => { reload(); }, [reload]);

  const remove = useCallback(async (p: ProjectRow) => {
    if (!(await confirm({
      title: `Delete “${p.name}”?`,
      content: p.storage_hint
        ? `This DESTROYS its working tree — ${p.storage_hint} — which is in nobody's backup. Refused while sessions are still bound to it.`
        : 'This DESTROYS its working tree, which is in nobody\'s backup. Refused while sessions are still bound to it.',
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
    { header: 'Sandbox', id: 'sandbox', width: 'growCollapse', minWidth: 90, maxWidth: 180, renderCell: p => <span className="list-clip" title={p.sandbox}>{p.sandbox}</span> },
    { header: 'Storage', id: 'storage', width: 'growCollapse', minWidth: 140, renderCell: p => <span className="list-clip" title={p.storage_hint}>{p.storage_hint}</span> },
    { header: 'Sessions', id: 'sessions', width: 'auto', renderCell: p => <span className="list-nowrap">{p.session_count ?? 0}</span> },
    // The list is newest first, so the date it sorts on is on the row.
    { header: 'Created', id: 'created', width: 'auto', renderCell: p => <span className="list-nowrap">{p.created_at ? shortTime(p.created_at) : ''}</span> },
    actionsColumn<ProjectRow>(p => (
      <RowMenu label={`Actions for ${p.name}`}>
        <ActionList.Item onSelect={() => { void act(p, 'stop'); }}>Stop sandbox</ActionList.Item>
        {/* Rebuild replaces the compute and keeps the tree — which a backend
            whose sandbox IS the storage cannot do, so it is not offered. */}
        {p.sandboxType === 'docker' && (
          <ActionList.Item variant="danger" onSelect={() => { void act(p, 'rebuild'); }}>Rebuild container</ActionList.Item>
        )}
        <ActionList.Item variant="danger" onSelect={() => { void remove(p); }}>Delete</ActionList.Item>
      </RowMenu>
    )),
  ], [remove, act, ownerOf, labelFor]);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="admin-projects-title">Projects</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          Every owner&apos;s working trees, and where each one&apos;s files live.
          Deleting a row DESTROYS its storage — the volume, or the sandbox
          holding it — so there is nothing left to reclaim afterwards.
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
