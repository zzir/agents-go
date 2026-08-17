import { useMemo, type RefObject } from 'react';
import { ActionList, AnchoredOverlay } from '@primer/react';
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
  // ready for what follows.
  insert: string;
  title: string;
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
    () => api.workflows.list() as Promise<{ id: string; name: string; description?: string }[]>,
  );
  return useMemo<SlashCommand[]>(() => [
    {
      id: 'plan', insert: '/plan ', match: 'plan', icon: ChecklistIcon,
      title: 'Plan', description: 'A plan before any change — this message runs in plan mode',
    },
    {
      id: 'plan-off', insert: '/plan off ', match: 'plan off', icon: ChecklistIcon,
      title: 'Plan off', description: 'Leave plan mode — this message runs unrestrained',
    },
    ...(workflows || []).map(w => ({
      id: 'workflow:' + w.id, insert: `/workflow ${w.name} `, match: 'workflow ' + w.name.toLowerCase(), icon: WorkflowIcon,
      title: `Workflow ${w.name}`, description: w.description || 'Run this workflow here, with a brief',
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

// SlashCommandPopup offers the commands above the composer while a slash
// prefix is being typed. It never takes focus — the person keeps typing, and
// the composer forwards the arrow, Enter and Escape keys — so it is an
// AnchoredOverlay with its focus management off; the highlighted row is
// the composer's activeIndex.
export function SlashCommandPopup({ anchorRef, open, commands, activeIndex, onPick, onClose }: {
  anchorRef: RefObject<HTMLElement | null>;
  open: boolean;
  commands: SlashCommand[];
  activeIndex: number;
  onPick: (cmd: SlashCommand) => void;
  onClose: () => void;
}) {
  return (
    <AnchoredOverlay
      renderAnchor={null}
      anchorRef={anchorRef}
      open={open && commands.length > 0}
      onClose={onClose}
      side="outside-top"
      align="start"
      width="medium"
      focusTrapSettings={{ disabled: true }}
      focusZoneSettings={{ disabled: true }}
      overlayProps={{ preventFocusOnOpen: true, role: 'listbox', 'aria-label': 'Commands', id: 'slash-commands' }}
    >
      <ActionList role="presentation">
        {commands.map((c, i) => (
          <ActionList.Item key={c.id} id={slashOptionID(i)} active={i === activeIndex} role="option" aria-selected={i === activeIndex}
            onSelect={() => onPick(c)} onMouseDown={e => e.preventDefault()}>
            <ActionList.LeadingVisual><c.icon size={16} /></ActionList.LeadingVisual>
            {c.title}
            <ActionList.Description variant="block">{c.description}</ActionList.Description>
          </ActionList.Item>
        ))}
      </ActionList>
    </AnchoredOverlay>
  );
}
