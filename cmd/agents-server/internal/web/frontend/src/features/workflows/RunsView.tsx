import { useEffect, useMemo, useRef, useState } from 'react';
import { Button, CounterLabel, Label, Stack } from '@primer/react';
import { Blankslate, DataTable, Table, type Column } from '@primer/react/experimental';
import { HistoryIcon, LinkExternalIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { TASK_KIND_WORKFLOW, type TaskRow } from '@/lib/protocol';
import { taskStateFromRow } from '@/lib/useAgentSocket';
import { itemDuration, taskItem, type BackgroundItem } from '@/lib/background';

// A run row: the execution as the Tasks panel would show it, plus the
// conversation it belongs to — the one fact a per-session list never needs.
interface RunRow extends BackgroundItem {
  sessionId: string;
  sessionName: string;
}

interface TaskWithSession extends TaskRow { session_name?: string }
interface TaskPage { items: TaskWithSession[]; total: number }

const PAGE_SIZE = 25;

// A status as a Label: live states in the accent/attention colors the task
// dot uses, terminal ones in success/danger/secondary.
const STATUS_VARIANT: Record<string, 'accent' | 'attention' | 'success' | 'danger' | 'secondary'> = {
  working: 'accent', input_required: 'attention', completed: 'success', failed: 'danger', cancelled: 'secondary',
};

function fmtStarted(ms?: number): string {
  if (!ms) return '';
  const d = new Date(ms);
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

// RunsView lists workflow executions across every conversation, newest first,
// a page at a time. version moves when any execution does (the socket hears
// every session's task.updated), so the page follows live work without
// polling; a row opens its conversation with the execution's detail in the
// Inspector.
export function RunsView({ version, onOpenRun }: { version: string; onOpenRun: (sessionId: string, taskId: string) => void }) {
  const [pageIndex, setPageIndex] = useState(0);
  const { data, loading, error, reload } = useApi<TaskPage>(
    () => api.tasks.list({ kind: TASK_KIND_WORKFLOW, limit: PAGE_SIZE, offset: pageIndex * PAGE_SIZE }) as Promise<TaskPage>,
    [pageIndex],
  );
  // A page change refetches through useApi's deps; a change of the signature
  // refetches the same page — and only a CHANGE, so neither doubles a fetch.
  // Only the FIRST page follows live work: a later page is history being
  // read, and refetching it by offset while new runs land at the top would
  // shift its rows under the reader.
  const seen = useRef(version);
  useEffect(() => {
    if (seen.current === version) return;
    seen.current = version;
    if (pageIndex === 0) reload();
  }, [version, reload, pageIndex]);

  const rows = useMemo<RunRow[]>(() => (data?.items || []).map(r => ({
    ...taskItem(taskStateFromRow(r)),
    sessionId: r.parent_session_id || '',
    sessionName: r.session_name || (r.parent_session_id || '').slice(0, 8),
  })), [data]);
  const total = data?.total || 0;

  // Live durations tick; a page with nothing live stands still.
  const anyLive = rows.some(r => r.status === 'working' || r.status === 'input_required');
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!anyLive) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [anyLive]);

  const columns: Column<RunRow>[] = [
    {
      header: 'Workflow', field: 'label', rowHeader: true, width: 'auto',
      renderCell: r => (
        <span className="hub-run-name">
          <span className="hub-clip hub-clip-name" title={r.label}>{r.label}</span>
          {(r.attempt || 1) > 1 && <CounterLabel>#{r.attempt}</CounterLabel>}
        </span>
      ),
    },
    {
      header: 'Status', id: 'status', width: 'growCollapse', minWidth: 180,
      renderCell: r => (
        <span className="hub-run-status" title={r.error || r.activity || undefined}>
          <span className="hub-run-badge"><Label variant={STATUS_VARIANT[r.status] || 'secondary'} size="small">{r.status.replace('_', ' ')}</Label></span>
          {r.activity && <span className="hub-clip hub-run-activity">{r.activity}</span>}
          {r.error && <span className="hub-clip hub-run-error">{r.error}</span>}
        </span>
      ),
    },
    {
      header: 'Conversation', field: 'sessionName', width: 'auto',
      renderCell: r => <span className="hub-clip hub-clip-session" title={r.sessionName}>{r.sessionName}</span>,
    },
    { header: 'Started', id: 'started', width: 'auto', renderCell: r => <span className="hub-nowrap">{fmtStarted(r.createdAt)}</span> },
    { header: 'Duration', id: 'duration', width: 'auto', align: 'end', renderCell: r => itemDuration(r, now) },
    {
      header: '', id: 'open', width: 'auto', align: 'end',
      renderCell: r => (
        <Button size="small" variant="invisible" trailingVisual={LinkExternalIcon}
          disabled={!r.sessionId} onClick={() => onOpenRun(r.sessionId, r.id)}>Open</Button>
      ),
    },
  ];

  // The first load: the table's own skeleton, not a bare header over nothing.
  if (loading && !data) {
    return (
      <Table.Container className="hub-runs">
        <Table.Skeleton aria-labelledby="hub-runs-title" columns={columns} rows={6} />
      </Table.Container>
    );
  }
  if (!loading && !error && total === 0) {
    return (
      <Blankslate>
        <Blankslate.Visual><HistoryIcon size={24} /></Blankslate.Visual>
        <Blankslate.Heading>No runs yet</Blankslate.Heading>
        <Blankslate.Description>
          Every execution of a workflow — started from a conversation, by its agent, or by a trigger — lists here
          with the conversation it reports back to.
        </Blankslate.Description>
      </Blankslate>
    );
  }
  return (
    <Stack gap="condensed">
      {error && <div className="wf-run-hint">Could not load runs: {error}</div>}
      {/* One line per run: every text cell clips with an ellipsis and carries
          the full text as its title; the Status column is the one that
          gives way (growCollapse), the others size to their clipped content. */}
      <Table.Container className="hub-runs">
        <DataTable aria-labelledby="hub-runs-title" data={rows} columns={columns} />
        {total > PAGE_SIZE && (
          <Table.Pagination aria-label="Runs pages" pageSize={PAGE_SIZE} totalCount={total}
            onChange={({ pageIndex: i }) => { if (i !== pageIndex) setPageIndex(i); }} />
        )}
      </Table.Container>
    </Stack>
  );
}
