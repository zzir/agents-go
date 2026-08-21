import { useState, type ComponentProps } from 'react';
import { TextInput } from '@primer/react';
import { EyeIcon, EyeClosedIcon } from '@primer/octicons-react';

/** A masked input with a reveal toggle — every secret field renders through
 * here so masking (and the way to peek) is never per-panel. */
export function SecretInput(props: Omit<ComponentProps<typeof TextInput>, 'type' | 'trailingAction'>) {
  const [show, setShow] = useState(false);
  return (
    <TextInput
      {...props}
      type={show ? 'text' : 'password'}
      trailingAction={
        <TextInput.Action
          icon={show ? EyeClosedIcon : EyeIcon}
          aria-label={show ? 'Hide value' : 'Show value'}
          onClick={() => setShow(s => !s)}
        />
      }
    />
  );
}
