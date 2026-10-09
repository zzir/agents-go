import { useEffect, useMemo } from 'react';
import { ActionList } from '@primer/react';
import { ChecklistIcon, WorkflowIcon, type Icon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';

// WORKFLOW_COMMAND leads a message that starts a workflow instead of a turn:
// "/workflow <name> <brief…>", the name as the hub lists it.
export const WORKFLOW_COMMAND = /^\/workflow\b[ \t]*/;

// SLASH_PREFIX matches a composer holding nothing but the start of a command
// ("/", "/wo"), which is when the commands are offered; a space closes the
// offer.
const SLASH_PREFIX = /^\/(\S*)$/;

// A SlashCommand is one thing the composer can be told to do with a leading
// slash: what to type, and how the offer describes it.
export interface SlashCommand {
  id: string;
  // What the composer holds once picked (the command and a trailing space);
  // trimmed, the row's label.
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

// matchCommands narrows the commands to the typed prefix: an empty query offers
// all, otherwise those whose match string contains it ("/w" and "/build" both
// reach "workflow build").
export function matchCommands(commands: SlashCommand[], query: string): SlashCommand[] {
  return query ? commands.filter(c => c.match.includes(query)) : commands;
}

// slashOptionID is the DOM id of the i-th offered command, for the composer's
// aria-activedescendant.
export function slashOptionID(i: number): string { return 'slash-command-' + i; }

// SlashCommandPopup offers the commands while a slash prefix is typed: a panel pinned
// above the composer's box by CSS (not an overlay), scrolling past its cap. It never
// takes focus — the composer forwards arrow/Enter/Escape and owns activeIndex.
export function SlashCommandPopup({ open, commands, activeIndex, onPick }: {
  open: boolean;
  commands: SlashCommand[];
  activeIndex: number;
  onPick: (cmd: SlashCommand) => void;
}) {
  const shown = open && commands.length > 0;
  // Keep the highlighted row inside the panel's own scroll (scrollIntoView
  // would nudge every ancestor).
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
