import { useCallback, useEffect, useMemo, useState } from 'react';
import { Button, Flash, PageHeader, Stack } from '@primer/react';
import { Blankslate, type Column } from '@primer/react/experimental';
import { LogIcon } from '@primer/octicons-react';
import { ListTable } from '@/components/ListTable';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';

type AuditRow = Omit<ApiSchemas['store.AuditEvent'], 'id'> & { id: string };

// Rows fetched per cursor step; the table pages over what is loaded.
const CHUNK = 200;

// AuditPanel: the audit log, newest first. Older chunks come on request,
// keyed on the last line's id (the server's `before` cursor).
export function AuditPanel() {
  const [rows, setRows] = useState<AuditRow[] | null>(null);
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const load = useCallback(async (before?: string) => {
    setBusy(true);
    try {
      const chunk = ((await api.auth.audit(CHUNK, before)) ?? []).map(e => ({ ...e, id: e.id || '' }));
      setRows(prev => before ? [...(prev ?? []), ...chunk] : chunk);
      setDone(chunk.length < CHUNK);
    } catch {
      setError('Failed to load the audit log.');
    } finally {
      setBusy(false);
    }
  }, []);
  useEffect(() => { void load(); }, [load]);

  const last = rows?.[rows.length - 1];

  const columns = useMemo<Column<AuditRow>[]>(() => [
    { header: 'Action', id: 'action', rowHeader: true, width: 'auto', renderCell: e => <code className="list-code">{e.action}</code> },
    { header: 'Resource', id: 'resource', width: 'growCollapse', minWidth: 160, renderCell: e => <code className="list-code list-clip" title={e.resource}>{e.resource}{e.detail ? ` (${e.detail})` : ''}</code> },
    { header: 'Actor', id: 'actor', width: 'auto', renderCell: e => <span className="list-clip" title={e.actor_email || e.actor_id}>{e.actor_email || e.actor_id}</span> },
    { header: 'When', id: 'when', width: 'auto', renderCell: e => <span className="list-nowrap">{shortTime(e.created_at)}</span> },
  ], []);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id="audit-title">Audit logs</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>
          <span>
            Every config change, approval, run start, terminal and login. Retention is
            <code> --audit-retention-days</code>, not a setting.
          </span>
        </PageHeader.Description>
      </PageHeader>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      <ListTable
        labelledBy="audit-title"
        rows={rows ?? []}
        columns={columns}
        loading={rows === null}
        search={{ placeholder: 'Search loaded entries', match: (e, q) => `${e.action} ${e.resource || ''} ${e.detail || ''} ${e.actor_email || ''}`.toLowerCase().includes(q) }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><LogIcon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>Nothing recorded yet</Blankslate.Heading>
            <Blankslate.Description>The first change, approval or login writes the first line.</Blankslate.Description>
          </Blankslate>
        )}
        footer={!done && last?.created_at ? (
          <div className="list-table-more">
            <Button size="small" disabled={busy} onClick={() => { void load(last.id); }}>Load older entries</Button>
          </div>
        ) : null}
      />
    </Stack>
  );
}

export default AuditPanel;
