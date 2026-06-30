import { type ReactNode } from 'react';
import { FormControl } from '@primer/react';

export function fc(label: string | null, input: ReactNode, hint?: string | null) {
  return (
    <FormControl>
      {label && <FormControl.Label>{label}</FormControl.Label>}
      {input}
      {hint && <FormControl.Caption>{hint}</FormControl.Caption>}
    </FormControl>
  );
}
