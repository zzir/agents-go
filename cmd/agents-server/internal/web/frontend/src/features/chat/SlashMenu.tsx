import { useEffect, useMemo } from 'react';
import { ActionList } from '@primer/react';
import { ChecklistIcon, WorkflowIcon, type Icon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';

// WORKFLOW_COMMAND leads a message that starts a workflow into this
// conversation instead of a turn: "/workflow <name> <brief…>". The name is
// the workflow's, as the hub lists it; everything after it is the brief.
export const WORKFLOW_COMMAND = /^\/workflow\b[ \t]*/;

// SLASH_PREFIX matches a composer that holds nothing but the start of a
// command — "/" or "/wo" — which is when the commands are offered. A space
// ends the command and closes the offer: what follows is the message.
const SLASH_PREFIX = /^\/(\S*)$/;

// A SlashCommand is one thing the composer can be told to do with a leading
// slash: what to type, and how the offer describes it.
export interface SlashCommand {
  id: string;
  // What the composer holds once picked — the command and a trailing space,
  // ready for what follows. Trimmed, it is the row's label too — the offer
  // shows what picking it types.
  insert: string;
  description: string;
  icon: Icon;
  // What the typed prefix is matched against ("plan", "workflow build").
  match: string;
}

// slashQuery is the command prefix the composer holds, or null when it holds
// anything else.
export function slashQuery(text: string): string | null {
  const m = SLASH_PREFIX.exec(text);
  return m ? m[1].toLowerCase() : null;
}

// useSlashCommands is every command the composer offers: plan mode, and one
// "/workflow <name>" per workflow on this server.
export function useSlashCommands(): SlashCommand[] {
  const { data: workflows } = useApi<{ id: string; name: string; description?: string }[]>(
    () => api.workflows.list() as Promise<{ id: string; name: string; description?: string }[]>, [], 'workflows',
  );
  return useMemo<SlashCommand[]>(() => [
    {
      id: 'plan', insert: '/plan ', match: 'plan', icon: ChecklistIcon,
      description: 'A plan before any change — this message runs in plan mode',
    },
    {
      id: 'plan-off', insert: '/plan off ', match: 'plan off', icon: ChecklistIcon,
      description: 'Leave plan mode — this message runs unrestrained',
    },
    ...(workflows || []).map(w => ({
      id: 'workflow:' + w.id, insert: `/workflow ${w.name} `, match: 'workflow ' + w.name.toLowerCase(), icon: WorkflowIcon,
      description: w.description || 'Run this workflow here, with a brief',
    })),
  ], [workflows]);
}

// matchCommands narrows the commands to the typed prefix: an empty query
// offers all, otherwise those whose match string contains it — "/w" and
// "/build" both reach "workflow build".
export function matchCommands(commands: SlashCommand[], query: string): SlashCommand[] {
  return query ? commands.filter(c => c.match.includes(query)) : commands;
}

// slashOptionID is the DOM id of the i-th offered command, for the composer's
// aria-activedescendant.
export function slashOptionID(i: number): string { return 'slash-command-' + i; }

// SlashCommandPopup offers the commands while a slash prefix is being typed:
// a panel pinned ABOVE the composer's box (positioned by CSS off the box, not
// by an overlay's fitting logic, so it sits in the same place whether the
// composer is at the bottom of a transcript or in the middle of a greeting)
// that scrolls past its cap instead of growing. It never takes focus — the
// person keeps typing, and the composer forwards the arrow, Enter and Escape
// keys; the highlighted row is the composer's activeIndex, kept in view.
export function SlashCommandPopup({ open, commands, activeIndex, onPick }: {
  open: boolean;
  commands: SlashCommand[];
  activeIndex: number;
  onPick: (cmd: SlashCommand) => void;
}) {
  const shown = open && commands.length > 0;
  // Keep the highlighted row inside the panel's own scroll — the panel's, not
  // scrollIntoView's, which would also nudge every scrolling ancestor.
  useEffect(() => {
    if (!shown) return;
    const row = document.getElementById(slashOptionID(activeIndex));
    const panel = row?.closest('.slash-popup');
    if (!row || !panel) return;
    const r = row.getBoundingClientRect();
    const p = panel.getBoundingClientRect();
    if (r.top < p.top) panel.scrollTop -= p.top - r.top;
    else if (r.bottom > p.bottom) panel.scrollTop += r.bottom - p.bottom;
  }, [shown, activeIndex]);
  if (!shown) return null;
  return (
    <div className="slash-popup" role="listbox" aria-label="Commands" id="slash-commands">
      <ActionList role="presentation">
        {commands.map((c, i) => (
          <ActionList.Item key={c.id} id={slashOptionID(i)} active={i === activeIndex} role="option" aria-selected={i === activeIndex}
            onSelect={() => onPick(c)} onMouseDown={e => e.preventDefault()}>
            <ActionList.LeadingVisual><c.icon size={16} /></ActionList.LeadingVisual>
            <span className="slash-cmd">{c.insert.trim()}</span>
            <ActionList.Description variant="inline" truncate>{c.description}</ActionList.Description>
          </ActionList.Item>
        ))}
      </ActionList>
    </div>
  );
}
