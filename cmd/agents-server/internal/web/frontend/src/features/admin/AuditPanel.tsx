import { useCallback, useEffect, useState } from 'react';
import { Button, Flash, PageHeader, Stack } from '@primer/react';
import { api, type ApiSchemas } from '@/lib/api';
import { shortTime } from '@/lib/format';

type AuditRow = ApiSchemas['store.AuditEvent'];

const PAGE = 50;

// AuditPanel: the audit log, newest first, older pages keyed on the last
// line's time (the server's `before` cursor).
export function AuditPanel() {
  const [rows, setRows] = useState<AuditRow[]>([]);
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const load = useCallback(async (before?: string) => {
    setBusy(true);
    try {
      const page = (await api.auth.audit(PAGE, before)) ?? [];
      setRows(prev => before ? [...prev, ...page] : page);
      setDone(page.length < PAGE);
    } catch {
      setError('Failed to load the audit log.');
    } finally {
      setBusy(false);
    }
  }, []);
  useEffect(() => { void load(); }, [load]);

  const last = rows[rows.length - 1];

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Audit logs</PageHeader.Title>
        </PageHeader.TitleArea>
      </PageHeader>
      <p className="account-muted">
        Who did what: every configuration change, approval decision, run start,
        terminal opened and login. Retention is the server's
        <code> --audit-retention-days</code>, not a setting.
      </p>
      {error ? <Flash variant="danger">{error}</Flash> : null}
      {rows.length === 0 && !busy ? (
        <span className="account-muted">Nothing recorded yet.</span>
      ) : (
        <Stack gap="none" className="account-pat-list">
          {rows.map(e => (
            <Stack key={e.id} direction="horizontal" align="center" gap="condensed" className="account-pat-row">
              <code className="account-secret" style={{ flexGrow: 1 }}>{e.action}{e.resource ? ` ${e.resource}` : ''}{e.detail ? ` (${e.detail})` : ''}</code>
              <span className="account-muted">{e.actor_email || e.actor_id} · {shortTime(e.created_at)}</span>
            </Stack>
          ))}
        </Stack>
      )}
      {!done && last?.created_at ? (
        <div>
          <Button size="small" disabled={busy} onClick={() => { void load(last.created_at); }}>Load older</Button>
        </div>
      ) : null}
    </Stack>
  );
}

export default AuditPanel;
