import { useEffect, useMemo, useState } from 'react';
import { Button, Link, PageHeader, Stack, TextInput } from '@primer/react';
import { Blankslate, DataTable, Table, type Column } from '@primer/react/experimental';
import { LogIcon, SearchIcon } from '@primer/octicons-react';
import { Loading } from '@/components/Loading';
import { api, type ApiSchemas } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { formatTime } from '@/lib/time';
import { toast } from '@/lib/toast';
import { useLoadError } from '@/features/admin/useLoadError';
import './admin.css';

type AuditRow = Omit<ApiSchemas['store.AuditEvent'], 'id'> & { id: string };

// Rows per cursor step.
const CHUNK = 100;

const fetchChunk = async (before?: string): Promise<AuditRow[]> =>
  ((await api.auth.audit(CHUNK, before)) ?? []).map(e => ({ ...e, id: e.id || '' }));

// The resource is a session id when the request addressed one (the REST
// route's first parameter, or a run created over the socket).
const isSessionAction = (action?: string) => !!action && (action === 'ws.run.create' || /\/sessions\/:id/.test(action));

function ResourceCell({ e }: { e: AuditRow }) {
  if (!e.resource) return null;
  const detail = e.detail ? ` (${e.detail})` : '';
  if (isSessionAction(e.action)) {
    return (
      <code className="list-code list-clip" title={e.resource}>
        <Link href={`#/session/${e.resource}`}>{e.resource.slice(0, 8)}</Link>{detail}
      </code>
    );
  }
  return <code className="list-code list-clip" title={e.resource}>{e.resource.slice(0, 8)}{detail}</code>;
}

// AuditPanel: the audit log, newest first, one paging model: the server's
// cursor (the last line's id as `before`) — every loaded line stays on the
// page, and "Load older entries" appends the next chunk.
export function AuditPanel() {
  const { data: first, error, loading } = useApi<AuditRow[]>(() => fetchChunk(), [], 'admin:audit');
  useLoadError(error, 'the audit log');
  // The chunks after the first; reset whenever the first is refetched.
  const [tail, setTail] = useState<{ rows: AuditRow[]; done: boolean }>({ rows: [], done: false });
  useEffect(() => { setTail({ rows: [], done: false }); }, [first]);
  const [busy, setBusy] = useState(false);
  const [query, setQuery] = useState('');

  const rows = useMemo(() => [...(first ?? []), ...tail.rows], [first, tail.rows]);
  const done = (first?.length ?? 0) < CHUNK || tail.done;
  const last = rows[rows.length - 1];

  const loadOlder = async () => {
    if (!last || busy) return;
    setBusy(true);
    try {
      const chunk = await fetchChunk(last.id);
      setTail(prev => ({ rows: [...prev.rows, ...chunk], done: chunk.length < CHUNK }));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to load older entries.');
    } finally {
      setBusy(false);
    }
  };

  const q = query.trim().toLowerCase();
  const filtered = useMemo(() => q
    ? rows.filter(e => `${e.action} ${e.resource || ''} ${e.detail || ''} ${e.actor_email || ''}`.toLowerCase().includes(q))
    : rows, [rows, q]);

  const columns = useMemo<Column<AuditRow>[]>(() => [
    { header: 'Action', id: 'action', rowHeader: true, width: 'auto', minWidth: 160, renderCell: e => <code className="list-code">{e.action}</code> },
    { header: 'Resource', id: 'resource', width: 'growCollapse', minWidth: 160, renderCell: e => <ResourceCell e={e} /> },
    { header: 'Actor', id: 'actor', width: 'auto', minWidth: 120, renderCell: e => <span className="list-clip" title={e.actor_email || e.actor_id}>{e.actor_email || e.actor_id}</span> },
    { header: 'When', id: 'when', width: 'auto', minWidth: 130, renderCell: e => <span className="list-nowrap">{e.created_at ? formatTime(e.created_at, 'long') : ''}</span> },
  ], []);

  let body;
  if (loading && rows.length === 0) {
    body = <Loading kind="list" />;
  } else if (rows.length === 0) {
    body = (
      <div className="list-table-empty">
        <Blankslate>
          <Blankslate.Visual><LogIcon size={24} /></Blankslate.Visual>
          <Blankslate.Heading>Nothing recorded yet</Blankslate.Heading>
          <Blankslate.Description>The first change, approval or login writes the first line.</Blankslate.Description>
        </Blankslate>
      </div>
    );
  } else if (filtered.length === 0) {
    body = <div className="list-table-empty"><Blankslate><Blankslate.Description>Nothing matches “{query.trim()}”.</Blankslate.Description></Blankslate></div>;
  } else {
    body = <DataTable aria-labelledby="audit-title" data={filtered} columns={columns} />;
  }

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
      <Table.Container className="list-table">
        {rows.length > 0 && (
          <div className="list-table-filter">
            <TextInput leadingVisual={SearchIcon} size="small" block aria-label="Search loaded entries" placeholder="Search loaded entries"
              value={query} onChange={e => setQuery(e.target.value)} />
          </div>
        )}
        {body}
        {rows.length > 0 && (
          <div className="list-table-more">
            <Stack direction="horizontal" align="center" gap="normal">
              <span className="list-table-count">{rows.length} loaded{done ? ' — that is everything' : ''}</span>
              {!done && <Button size="small" disabled={busy} onClick={() => { void loadOlder(); }}>{busy ? 'Loading…' : 'Load older entries'}</Button>}
            </Stack>
          </div>
        )}
      </Table.Container>
    </Stack>
  );
}

export default AuditPanel;
