import { type ReactNode } from 'react';
import { FormControl, SegmentedControl } from '@primer/react';

// hideLabel keeps the label for the accessibility tree but off the screen —
// for a control whose group title already names it visually. Primer requires
// every FormControl input to have a Label child, so "no label" is not an
// option, only a hidden one.
export function fc(label: string | null, input: ReactNode, hint?: string | null, opts?: { hideLabel?: boolean }) {
  return (
    <FormControl>
      {label && <FormControl.Label visuallyHidden={opts?.hideLabel}>{label}</FormControl.Label>}
      {input}
      {hint && <FormControl.Caption>{hint}</FormControl.Caption>}
    </FormControl>
  );
}

/** A labeled horizontal single-choice row — the segmented replacement for a
 * short Select. Every option is visible at a glance, which a dropdown hides
 * behind a click; use it when the option set is small and fixed. */
export function seg(
  label: string,
  value: string,
  options: readonly (readonly [value: string, text: string])[],
  onChange: (v: string) => void,
  hint?: string | null,
) {
  return fc(label, (
    <SegmentedControl aria-label={label} size="small">
      {options.map(([v, text]) => (
        <SegmentedControl.Button key={v || 'default'} selected={value === v} onClick={() => onChange(v)}>
          {text}
        </SegmentedControl.Button>
      ))}
    </SegmentedControl>
  ), hint);
}
