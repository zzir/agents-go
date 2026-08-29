import { useState } from 'react';
import { ActionList, ActionMenu, Button, Dialog, Flash, IconButton, Label, Select, Stack, TextInput, Textarea, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { ClockIcon, DependabotIcon, KebabHorizontalIcon, TrashIcon, WebhookIcon, WorkflowIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi, useCopy } from '@/lib/hooks';
import { nameOf, type Named } from '@/lib/named';
import { fc } from '@/lib/form';
import { toast } from '@/lib/toast';
import { BADGE } from '@/lib/badges';
import { Disclosure } from '@/components/Disclosure';
import { SessionPicker, UnboundHint } from '@/features/sessions/SessionPicker';
import { shortTime } from '@/lib/format';

// A trigger starts work without a conversation asking — on a cron schedule,
// or on a signed webhook call — into the session it names, with the brief its
// author wrote in advance. What it starts is its target: a workflow (an
// execution that reports back to the session) or an agent turn (the brief
// sent as a message of the session, run by that agent).
export interface Trigger {
  id: string;
  target: 'workflow' | 'agent';
  workflow_id?: string;
  agent_config_id?: string;
  session_id: string;
  kind: 'cron' | 'webhook';
  brief: string;
  schedule?: string;
  enabled: boolean;
  last_fired_at?: string;
  // The task (a workflow) or run (an agent turn) the last fire started.
  last_started_id?: string;
  last_error?: string;
  // The secret is in the response that minted it (create, rotate) and never
  // again; the hint is its tail.
  secret?: string;
  secret_hint?: string;
  hook_path?: string;
}

interface TriggerForm {
  target: 'workflow' | 'agent';
  workflow_id: string;
  agent_config_id: string;
  kind: 'cron' | 'webhook';
  schedule: string;
  session_id: string;
  brief: string;
}

const SCHEDULE_HINT = 'Five fields (0 9 * * 1-5), or @hourly / @daily / @every 30m (a minute at least). Ticks missed while the server is down are not replayed.';

// SecretBox shows a freshly minted webhook secret, once, with what a caller
// needs: the URL, the two headers, and a signing example.
export function SecretBox({ trigger }: { trigger: Trigger }) {
  const { copied, copy: copyText } = useCopy();
  const url = window.location.origin + (trigger.hook_path || '');
  const copy = (what: string, text: string) => copyText(text, what);
  const example = [
    `TS=$(date +%s)`,
    `BODY='{"event":"example"}'`,
    `SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac '${trigger.secret}' | sed 's/^.* //')`,
    `curl -X POST '${url}' -H "X-Timestamp: $TS" -H "X-Signature-256: $SIG" -d "$BODY"`,
  ].join('\n');
  return (
    <div className="wf-secret-box">
      <div className="wf-secret-title">Signing secret — shown once. Copy it now.</div>
      <div className="wf-secret-row"><code>{trigger.secret}</code>
        <Button size="small" onClick={() => copy('secret', trigger.secret || '')}>{copied === 'secret' ? 'Copied' : 'Copy'}</Button></div>
      <div className="wf-secret-row"><code>POST {url}</code>
        <Button size="small" onClick={() => copy('url', url)}>{copied === 'url' ? 'Copied' : 'Copy'}</Button></div>
      <div className="wf-run-hint">
        Send <code>X-Timestamp</code> (UNIX seconds, within 5 minutes) and <code>X-Signature-256</code> =
        hex HMAC-SHA256(secret, timestamp + "." + body). The body is appended to the brief as the payload.
      </div>
      <pre className="wf-secret-example">{example}</pre>
      <Button size="small" onClick={() => copy('example', example)}>{copied === 'example' ? 'Copied' : 'Copy example'}</Button>
    </div>
  );
}

// useTriggerActions is what every trigger list does with a row — enable /
// disable, fire, rotate the secret, delete — with one busy flag per trigger
// (two worked at once must not free each other's buttons) and the secret a
// rotation just minted, until the caller drops it.
export function useTriggerActions(reload: () => void, sessionName: (id: string) => string) {
  const confirm = useConfirm();
  const [busy, setBusy] = useState<Set<string>>(() => new Set());
  const [minted, setMinted] = useState<Trigger | null>(null);
  const run = async (what: string, fn: () => Promise<unknown>, done?: string) => {
    setBusy(prev => new Set(prev).add(what));
    try {
      await fn();
      if (done) toast.success(done);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Request failed');
    } finally {
      setBusy(prev => { const next = new Set(prev); next.delete(what); return next; });
    }
  };
  return {
    busy: (what: string) => busy.has(what),
    run,
    minted,
    setMinted,
    toggle: (t: Trigger) => run(t.id, () => api.triggers.update(t.id, { ...t, enabled: !t.enabled })),
    fire: (t: Trigger) => run(t.id, async () => {
      const fired = await api.triggers.fire(t.id) as { task_id?: string; run_id?: string };
      const what = fired.task_id ? `task ${fired.task_id.slice(0, 8)}` : `run ${(fired.run_id || '').slice(0, 8)}`;
      toast.success(`Fired — ${what} started in "${sessionName(t.session_id)}"`);
    }),
    rotate: (t: Trigger) => run(t.id, async () => { setMinted(await api.triggers.rotateSecret(t.id) as Trigger); }, 'Secret rotated — the old one no longer works'),
    remove: async (t: Trigger) => {
      if (!await confirm({ title: 'Delete trigger?', content: 'It stops firing at once. Work it already started keeps running.', confirmButtonContent: 'Delete', confirmButtonType: 'danger' })) return;
      run(t.id, () => api.triggers.delete(t.id), 'Trigger deleted');
    },
  };
}

// TriggerRow is one trigger as every list shows it: one line — the status
// dot and what it starts as the title (a workflow or an agent, said by the
// icon), how it fires as a label — that opens on the rest: where it fires
// and into which conversation, its brief, how it last went. Two actions stay
// in reach — fire, and the switch — the rest sit behind the kebab; their box
// stops the clicks (the kebab's menu included — it bubbles through the React
// tree from its portal) so none of them toggles the row. targetName is the
// workflow's or the agent's; a list under one workflow passes none, and the
// kind takes the title's place.
export function TriggerRow({ t, sessionName, targetName, actions }:
  { t: Trigger; sessionName: string; targetName?: string; actions: ReturnType<typeof useTriggerActions> }) {
  const busy = actions.busy(t.id);
  const started = t.last_started_id ? ` · ${t.target === 'agent' ? 'run' : 'task'} ${t.last_started_id.slice(0, 8)}` : '';
  // The dot: what the last fire says — red for an error, green for a fire
  // that went, muted while it has never fired or is switched off.
  const dot = !t.enabled ? 'var(--fgColor-muted)' : t.last_error ? 'var(--fgColor-danger)' : t.last_fired_at ? 'var(--fgColor-success)' : 'var(--fgColor-muted)';
  const TargetIcon = t.target === 'agent' ? DependabotIcon : WorkflowIcon;
  const KindIcon = t.kind === 'cron' ? ClockIcon : WebhookIcon;
  return (
    <Disclosure as="div" variant="plain" className="disclosure-row hub-row" label={<>
      <div className="resource-row-main">
        <div className="resource-row-head">
          <span className="form-status-dot" style={{ background: dot }} title={!t.enabled ? 'off' : t.last_error ? 'last fire failed' : t.last_fired_at ? 'last fire went' : 'never fired'} />
          {targetName ? (
            <>
              <span className="resource-row-title wf-trigger-title"><TargetIcon size={14} />{targetName}</span>
              <Label variant={t.kind === 'cron' ? BADGE.type : 'accent'}>{t.kind}</Label>
            </>
          ) : (
            <span className="resource-row-title wf-trigger-title"><KindIcon size={14} />{t.kind === 'cron' ? 'Cron' : 'Webhook'}</span>
          )}
          {!t.enabled && <Label variant="secondary">off</Label>}
        </div>
      </div>
      <div className="resource-row-actions" onClick={e => e.stopPropagation()}>
        <Button size="small" variant="invisible" disabled={busy || !t.enabled} onClick={() => actions.fire(t)}>Fire now</Button>
        <Button size="small" variant="invisible" disabled={busy} onClick={() => actions.toggle(t)}>{t.enabled ? 'Disable' : 'Enable'}</Button>
        <ActionMenu>
          <ActionMenu.Anchor>
            <IconButton icon={KebabHorizontalIcon} size="small" variant="invisible" aria-label="More" disabled={busy} />
          </ActionMenu.Anchor>
          <ActionMenu.Overlay>
            <ActionList>
              {t.kind === 'webhook' && <ActionList.Item onSelect={() => actions.rotate(t)}>Rotate secret</ActionList.Item>}
              <ActionList.Item variant="danger" onSelect={() => actions.remove(t)}>
                <ActionList.LeadingVisual><TrashIcon size={16} /></ActionList.LeadingVisual>
                Delete
              </ActionList.Item>
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      </div>
    </>}>
      <div className="hub-row-detail wf-trigger-detail">
        <div className="resource-row-meta">
          <span><KindIcon size={12} /> <code className="wf-trigger-when">{t.kind === 'cron' ? t.schedule : (t.hook_path || '')}</code></span>
          <span>→ {sessionName}</span>
          {t.kind === 'webhook' && t.secret_hint && <span>· secret {t.secret_hint}</span>}
        </div>
        {t.brief && <div className="wf-trigger-brief">{t.brief}</div>}
        <div className={'wf-trigger-last' + (t.last_error ? ' wf-trigger-error' : '')}>
          {t.last_error ? `last fire failed: ${t.last_error}` : t.last_fired_at ? `last fired ${shortTime(t.last_fired_at)}${started}` : 'never fired'}
        </div>
      </div>
    </Disclosure>
  );
}

// TriggerForm creates one trigger: what to start (a workflow, or a turn of an
// agent — fixed when the form belongs to one workflow), what fires it, where,
// and the brief. onCreated gets the row (a webhook's with its secret, once).
export function TriggerForm({ fixedWorkflow, sessionId, onCreated, onCancel }:
  { fixedWorkflow?: Named | null; sessionId: string | null; onCreated: (t: Trigger) => void; onCancel: () => void }) {
  const { data: workflows } = useApi<Named[]>(() => fixedWorkflow ? Promise.resolve([fixedWorkflow]) : api.workflows.list() as Promise<Named[]>);
  const { data: agents } = useApi<Named[]>(() => api.agents.list() as Promise<Named[]>);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState<TriggerForm>({
    target: 'workflow', workflow_id: fixedWorkflow?.id || '', agent_config_id: '',
    kind: 'cron', schedule: '', session_id: sessionId || '', brief: '',
  });
  const set = (patch: Partial<TriggerForm>) => setForm(f => ({ ...f, ...patch }));

  const ready = form.session_id
    && (form.target === 'workflow' ? form.workflow_id : form.agent_config_id)
    && (form.kind !== 'cron' || form.schedule.trim());
  const create = async () => {
    setBusy(true);
    try {
      const body = {
        target: form.target, kind: form.kind, schedule: form.schedule, session_id: form.session_id, brief: form.brief, enabled: true,
        ...(form.target === 'workflow' ? { workflow_id: form.workflow_id } : { agent_config_id: form.agent_config_id }),
      };
      onCreated(await api.triggers.create(body) as Trigger);
      toast.success('Trigger added');
    } catch (e) {
      toast.error((e as Error).message || 'Could not add the trigger');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="wf-trigger-form">
      <Stack gap="condensed">
        {fixedWorkflow ? (
          fc('Starts', <TextInput block disabled value={`workflow ${fixedWorkflow.name || fixedWorkflow.id.slice(0, 8)}`} />)
        ) : (
          <>
            {fc('Starts', <Select block value={form.target} onChange={e => set({ target: e.target.value as TriggerForm['target'] })}>
              <Select.Option value="workflow">A workflow — an execution that reports back to the conversation</Select.Option>
              <Select.Option value="agent">An agent turn — the brief sent as a message of the conversation</Select.Option>
            </Select>)}
            {form.target === 'workflow'
              ? fc('Workflow', <Select block value={form.workflow_id} onChange={e => set({ workflow_id: e.target.value })}>
                  <Select.Option value="">Select a workflow…</Select.Option>
                  {(workflows || []).map(w => <Select.Option key={w.id} value={w.id}>{w.name || w.id.slice(0, 8)}</Select.Option>)}
                </Select>)
              : fc('Agent', <Select block value={form.agent_config_id} onChange={e => set({ agent_config_id: e.target.value })}>
                  <Select.Option value="">Select an agent…</Select.Option>
                  {(agents || []).map(a => <Select.Option key={a.id} value={a.id}>{a.name || a.id.slice(0, 8)}</Select.Option>)}
                </Select>, 'Runs the brief as an ordinary turn, with the conversation’s own sandbox')}
          </>
        )}
        {fc('Fires', <Select block value={form.kind} onChange={e => set({ kind: e.target.value as TriggerForm['kind'] })}>
          <Select.Option value="cron">On a schedule (cron)</Select.Option>
          <Select.Option value="webhook">When called (webhook, signed)</Select.Option>
        </Select>)}
        {form.kind === 'cron' && fc('Schedule', <TextInput block value={form.schedule} placeholder="0 9 * * 1-5"
          onChange={e => set({ schedule: e.target.value })} />, SCHEDULE_HINT)}
        {fc('Conversation', <SessionPicker value={form.session_id} onChange={id => set({ session_id: id })} />,
          form.target === 'agent' ? 'Where the turn happens' : 'Where each run reports back')}
        <UnboundHint key={form.session_id} sessionId={form.session_id} what={form.target === 'agent' ? 'the turn' : 'each run'} />
        {fc('Brief', <Textarea block rows={8} value={form.brief}
          placeholder={form.target === 'agent' ? 'The message to send each time — say everything the agent needs' : 'What each run is about — it cannot see the conversation, so say everything it needs'}
          onChange={e => set({ brief: e.target.value })} />,
          form.kind === 'webhook' ? 'The call’s body is appended as the payload' : null)}
        <Stack direction="horizontal" gap="condensed">
          <Button variant="primary" size="small" onClick={create} disabled={busy || !ready}>
            {busy ? 'Adding…' : 'Add trigger'}
          </Button>
          <Button size="small" onClick={onCancel}>Cancel</Button>
        </Stack>
      </Stack>
    </div>
  );
}

// TriggersDialog is one workflow's triggers, from its row in the Definitions
// list: the same rows and form as the hub's Triggers view, fixed to it.
export function TriggersDialog({ workflowId, workflowName, sessionId, onClose }:
  { workflowId: string; workflowName: string; sessionId: string | null; onClose: () => void }) {
  const { data: triggers, loading, error, reload } = useApi<Trigger[]>(() => api.triggers.listFor(workflowId) as Promise<Trigger[]>, [workflowId]);
  const { data: sessions } = useApi<Named[]>(() => api.sessions.list() as Promise<Named[]>);
  const [adding, setAdding] = useState(false);
  const sessionName = (id: string) => nameOf(sessions, id);
  const actions = useTriggerActions(reload, sessionName);

  return (
    <Dialog title={`Triggers · ${workflowName}`} onClose={onClose} width="xlarge"
      footerButtons={[{ buttonType: 'default', content: 'Close', onClick: onClose }]}>
      <Stack gap="normal">
        <div className="wf-run-hint">
          A trigger runs this workflow without anyone asking — on a schedule, or when something calls its webhook —
          into the conversation you pick, with the brief you write here. The result comes back there like any run's.
        </div>

        {actions.minted && <SecretBox trigger={actions.minted} />}

        {loading && <div className="wf-run-hint">Loading…</div>}
        {error && <Flash variant="danger">Could not load triggers: {error}</Flash>}
        {(triggers || []).length > 0 && (
          <div className="Box">
            {(triggers || []).map(t => <TriggerRow key={t.id} t={t} sessionName={sessionName(t.session_id)} actions={actions} />)}
          </div>
        )}
        {!loading && !error && (triggers || []).length === 0 && !adding && (
          <Blankslate>
            <Blankslate.Description>No triggers yet.</Blankslate.Description>
          </Blankslate>
        )}

        {adding ? (
          <TriggerForm fixedWorkflow={{ id: workflowId, name: workflowName }} sessionId={sessionId} onCancel={() => setAdding(false)}
            onCreated={t => { if (t.kind === 'webhook') actions.setMinted(t); setAdding(false); reload(); }} />
        ) : (
          <div><Button size="small" onClick={() => setAdding(true)}>+ Add trigger</Button></div>
        )}
      </Stack>
    </Dialog>
  );
}
