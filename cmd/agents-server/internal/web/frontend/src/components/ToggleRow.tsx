import { useId, type ReactNode } from 'react';
import { ToggleSwitch } from '@primer/react';
import { useReadOnly } from '@/lib/access';
import './toggle-row.css';

/** One boolean as GitHub's settings show it: the name and what it does on the
 * left, the On/Off switch on the right, in a bordered row of its own. A
 * read-only dialog disables it here — the fieldset that disables every other
 * input never reaches the switch's status text. */
export function ToggleRow({ label, description, checked, onChange, disabled }: {
  label: string;
  description?: ReactNode;
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
}) {
  const id = useId();
  const readOnly = useReadOnly();
  return (
    <div className="toggle-row">
      <div className="toggle-row-main">
        <div id={`${id}-label`} className="toggle-row-title">{label}</div>
        {description && <div id={`${id}-desc`} className="toggle-row-desc">{description}</div>}
      </div>
      {/* onClick, not Primer's onChange: controlled, that one re-fires on every
          `checked` change, mount included — a store on load, not on a click. */}
      <ToggleSwitch
        size="small"
        checked={checked}
        onClick={() => onChange(!checked)}
        disabled={disabled || readOnly}
        aria-labelledby={`${id}-label`}
        aria-describedby={description ? `${id}-desc` : undefined}
      />
    </div>
  );
}
