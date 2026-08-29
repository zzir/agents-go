import { useState } from 'react';
import { ActionMenu, Button, Tooltip } from '@primer/react';
import { AgentAvatar } from '@/components/AgentAvatar';
import { AVATARS } from '@/lib/avatars';

// AvatarPicker: the button beside the Name input, opening the built-in avatar
// catalog as a grid. Selecting only changes the form value — Save persists it.
export function AvatarPicker({ name, value, onChange }: { name: string; value: string; onChange: (path: string) => void }) {
  const [open, setOpen] = useState(false);
  const pick = (path: string) => { onChange(path); setOpen(false); };
  return (
    <ActionMenu open={open} onOpenChange={setOpen}>
      <ActionMenu.Anchor>
        <Button aria-label="Choose avatar" className="avatar-picker-button">
          <AgentAvatar name={name} avatar={value} size={24} />
        </Button>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="auto">
        <div className="avatar-grid" role="listbox" aria-label="Built-in avatars">
          <Tooltip text="No avatar" direction="s" type="label">
            <button type="button" role="option" aria-selected={value === ''}
              className={'avatar-grid-cell' + (value === '' ? ' avatar-grid-cell--selected' : '')}
              onClick={() => pick('')}>
              <AgentAvatar name={name} size={32} />
            </button>
          </Tooltip>
          {AVATARS.map(a => (
            <Tooltip key={a.path} text={a.label} direction="s" type="label">
              <button type="button" role="option" aria-selected={value === a.path}
                className={'avatar-grid-cell' + (value === a.path ? ' avatar-grid-cell--selected' : '')}
                onClick={() => pick(a.path)}>
                <img className="user-avatar" style={{ width: 32, height: 32 }} src={a.path} alt="" />
              </button>
            </Tooltip>
          ))}
        </div>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}
