import { useState } from 'react';
import { Button, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { ZapIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { SecretBox, TriggerForm, TriggerRow, useTriggerActions, type Named, type Trigger } from '@/features/workflows/TriggersDialog';

// TriggersView is every trigger in one list — what starts on its own, a
// workflow or an agent's turn, and how it last went — with the form to add
// one of either kind.
export function TriggersView({ sessionId }: { sessionId: string | null }) {
  const { data: triggers, loading, error, reload } = useApi<Trigger[]>(() => api.triggers.list() as Promise<Trigger[]>);
  const { data: workflows } = useApi<Named[]>(() => api.workflows.list() as Promise<Named[]>);
  const { data: agents } = useApi<Named[]>(() => api.agents.list() as Promise<Named[]>);
  const { data: sessions } = useApi<Named[]>(() => api.sessions.list() as Promise<Named[]>);
  const [adding, setAdding] = useState(false);

  const nameOf = (list: Named[] | null, id: string) => (list || []).find(x => x.id === id)?.name || id.slice(0, 8);
  const sessionName = (id: string) => nameOf(sessions, id);
  const targetName = (t: Trigger) => t.target === 'agent' ? nameOf(agents, t.agent_config_id || '') : nameOf(workflows, t.workflow_id || '');
  const actions = useTriggerActions(reload, sessionName);

  const list = triggers || [];
  return (
    <Stack gap="normal">
      <div className="hub-toolbar">
        <div className="wf-run-hint">
          What runs without anyone asking — a workflow, or a turn of an agent — on a schedule or when its webhook is called, and how it last went.
        </div>
        {!adding && <Button size="small" leadingVisual={ZapIcon} onClick={() => setAdding(true)}>Add trigger</Button>}
      </div>

      {adding && (
        <TriggerForm sessionId={sessionId} onCancel={() => setAdding(false)}
          onCreated={t => { if (t.kind === 'webhook') actions.setMinted(t); setAdding(false); reload(); }} />
      )}
      {actions.minted && <SecretBox trigger={actions.minted} />}
      {loading && <div className="wf-run-hint">Loading…</div>}
      {error && <div className="wf-run-hint">Could not load triggers: {error}</div>}
      {list.length > 0 && (
        <div className="Box">
          {list.map(t => (
            <TriggerRow key={t.id} t={t} sessionName={sessionName(t.session_id)} targetName={targetName(t)} actions={actions} />
          ))}
        </div>
      )}
      {!loading && !error && list.length === 0 && !adding && (
        <Blankslate>
          <Blankslate.Visual><ZapIcon size={24} /></Blankslate.Visual>
          <Blankslate.Heading>No triggers yet</Blankslate.Heading>
          <Blankslate.Description>
            A trigger starts a workflow, or sends an agent a message, into a conversation of your choice — on a cron
            schedule or from a signed webhook, with a brief written in advance.
          </Blankslate.Description>
        </Blankslate>
      )}
    </Stack>
  );
}
