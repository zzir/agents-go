import { ActionMenu, ActionList, Label } from '@primer/react';
import { AgentAvatar } from './AgentAvatar';
import { BADGE } from '@/lib/badges';

export interface PickableAgent { id: string | number; name?: string; avatar?: string; scope?: string }

const labelOf = (a: PickableAgent) => a.name || String(a.id).slice(0, 8);

/** The names more than one agent in the list carries — a private agent and a
 * global one can share a name, and a picker must tell them apart. */
export function collidingNames(agents: PickableAgent[]): Set<string> {
  const seen = new Set<string>();
  const dup = new Set<string>();
  for (const a of agents) {
    const n = labelOf(a);
    if (seen.has(n)) dup.add(n); else seen.add(n);
  }
  return dup;
}

/** The scope badge a picker row shows only when its name collides. */
export function ScopeHint({ agent, colliding }: { agent: PickableAgent; colliding: Set<string> }) {
  if (!colliding.has(labelOf(agent))) return null;
  return <Label size="small" variant={BADGE.scope}>{agent.scope === 'global' ? 'Global' : 'Private'}</Label>;
}

// AgentPicker: the single-select agent dropdown, everywhere one is picked —
// an ActionMenu rather than a native <select> so each row shows its avatar.
export function AgentPicker({ agents, value, onChange, placeholder = 'Select an agent…', emptyLabel, ariaLabel = 'Agent', size, block, className }: {
  agents: PickableAgent[];
  value: string;
  onChange: (id: string) => void;
  placeholder?: string;
  // When set, "" is offered as a real option under this label (e.g. a global
  // scope); when unset, "" only renders as the placeholder.
  emptyLabel?: string;
  ariaLabel?: string;
  size?: 'small' | 'medium' | 'large';
  block?: boolean;
  className?: string;
}) {
  const selected = agents.find(a => String(a.id) === value);
  const colliding = collidingNames(agents);
  return (
    <ActionMenu>
      <ActionMenu.Button aria-label={ariaLabel} size={size} block={block} className={className}
        leadingVisual={selected ? () => <AgentAvatar name={selected.name} avatar={selected.avatar} size={20} /> : undefined}>
        {selected ? labelOf(selected) : (value === '' && emptyLabel) || placeholder}
      </ActionMenu.Button>
      <ActionMenu.Overlay>
        <ActionList selectionVariant="single">
          {emptyLabel && (
            <ActionList.Item selected={value === ''} onSelect={() => onChange('')}>{emptyLabel}</ActionList.Item>
          )}
          {agents.map(a => (
            <ActionList.Item key={a.id} selected={String(a.id) === value} onSelect={() => onChange(String(a.id))}>
              <ActionList.LeadingVisual>
                <AgentAvatar name={a.name} avatar={a.avatar} size={20} />
              </ActionList.LeadingVisual>
              {labelOf(a)}
              {colliding.has(labelOf(a)) && (
                <ActionList.TrailingVisual><ScopeHint agent={a} colliding={colliding} /></ActionList.TrailingVisual>
              )}
            </ActionList.Item>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}
