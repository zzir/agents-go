import { useState } from 'react';
import { Button, Stack } from '@primer/react';
import { api } from '@/lib/api';
import { PAGE_SIZE, useApi, usePage } from '@/lib/hooks';
import { Paged } from '@/components/Paged';
import { SecretBox, TriggerForm, TriggerListState, TriggerRow, useTriggerActions, type Trigger } from '@/features/workflows/TriggersDialog';
import { useServerInfo } from '@/features/settings/serverInfo';
import { nameOf, type Named } from '@/lib/named';

// TriggersView is every trigger in one list — what starts on its own, a
// workflow or an agent's turn, and how it last went — with the form to add
// one of either kind.
export function TriggersView({ sessionId }: { sessionId: string | null }) {
  const { data: triggers, loading, error, reload } = useApi<Trigger[]>(() => api.triggers.list() as Promise<Trigger[]>, [], 'triggers');
  const { data: workflows } = useApi<Named[]>(() => api.workflows.list() as Promise<Named[]>, [], 'workflows');
  const { data: agents } = useApi<Named[]>(() => api.agents.list() as Promise<Named[]>, [], 'agents');
  const { data: sessions } = useApi<Named[]>(() => api.sessions.list() as Promise<Named[]>, [], 'sessions');
  const { data: server } = useServerInfo();
  const [adding, setAdding] = useState(false);

  const sessionName = (id: string) => nameOf(sessions, id);
  const targetName = (t: Trigger) => t.target === 'agent' ? nameOf(agents, t.agent_config_id || '') : nameOf(workflows, t.workflow_id || '');
  const targetAvatar = (t: Trigger) => t.target === 'agent' ? (agents || []).find(a => a.id === t.agent_config_id)?.avatar : undefined;
  const actions = useTriggerActions(reload, sessionName);
  const tz = server?.timezone;

  const list = triggers || [];
  const page = usePage(list, PAGE_SIZE);
  return (
    <Stack gap="normal">
      <div className="hub-toolbar">
        <div className="wf-run-hint">
          What runs on its own — a workflow or an agent turn, on a schedule or a webhook call — and how it last went.
        </div>
        {!adding && <Button variant="primary" size="small" onClick={() => setAdding(true)}>+ Add</Button>}
      </div>

      {adding && (
        <TriggerForm sessionId={sessionId} timezone={tz} onCancel={() => setAdding(false)}
          onSaved={(t, created) => { if (created && t.kind === 'webhook') actions.setMinted(t); setAdding(false); reload(); }} />
      )}
      {actions.minted && <SecretBox trigger={actions.minted} />}
      <TriggerListState loading={loading} error={error} count={list.length} adding={adding} />
      {list.length > 0 && (
        <Paged page={page} total={list.length} label="Trigger pages">
          <div className="Box">
            {page.items.map(t => actions.editing?.id === t.id
              ? <TriggerForm key={t.id} inline sessionId={sessionId} initial={t} timezone={tz}
                  onCancel={() => actions.setEditing(null)} onSaved={() => { actions.setEditing(null); reload(); }} />
              : <TriggerRow key={t.id} t={t} sessionName={sessionName(t.session_id)} targetName={targetName(t)} targetAvatar={targetAvatar(t)} timezone={tz} actions={actions} />)}
          </div>
        </Paged>
      )}
    </Stack>
  );
}
