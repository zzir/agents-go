import { ActionList, ActionMenu, IconButton } from '@primer/react';

// Octicons has no slash glyph, and what this opens is typed as "/" — so the
// button draws the character rather than approximating it with another icon.
function SlashIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"
      fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <path d="M10.75 2.5 5.25 13.5" />
    </svg>
  );
}

// SlashMenu is the pointing half of the composer's slash commands: what can be
// typed as "/plan" is checkable here, for someone who does not know the command
// exists. Plan mode is a RESTRAINT on the agent, so entering it is the person's
// call and never the model's.
//
// Checking the box writes nothing — the phase travels with the message that
// runs under it, which is what keeps setting it and starting the run one step.
export function SlashMenu({ planning, onChange, running }: { planning: boolean; onChange: (planning: boolean) => void; running: boolean }) {
  return (
    <ActionMenu>
      <ActionMenu.Anchor>
        {/* Armed = visibly armed: the accent tint is the only always-on-screen
            sign the next message runs in plan mode. */}
        <IconButton icon={SlashIcon} variant="invisible" size="small" aria-label="Commands"
          className={planning ? 'slash-armed' : undefined}
          aria-description={planning ? 'Plan mode is on' : undefined} />
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="medium">
        <ActionList selectionVariant="multiple">
          <ActionList.Item selected={planning} disabled={running} onSelect={() => onChange(!planning)}>
            Plan mode
          </ActionList.Item>
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}
