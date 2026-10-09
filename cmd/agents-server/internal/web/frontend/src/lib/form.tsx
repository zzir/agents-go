import { type ReactNode } from 'react';
import { FormControl, SegmentedControl } from '@primer/react';

// hideLabel keeps the label for the accessibility tree but off screen, for a
// control its group title already names; Primer requires a Label child, so
// hidden is the only "no label".
export function fc(label: string | null, input: ReactNode, hint?: string | null, opts?: { hideLabel?: boolean }) {
  return (
    <FormControl>
      {label && <FormControl.Label visuallyHidden={opts?.hideLabel}>{label}</FormControl.Label>}
      {input}
      {hint && <FormControl.Caption>{hint}</FormControl.Caption>}
    </FormControl>
  );
}

/** A labeled horizontal single-choice row, the segmented replacement for a short
 * Select: every option visible at a glance. For a small, fixed option set. */
export function seg(
  label: string,
  value: string,
  options: readonly (readonly [value: string, text: string])[],
  onChange: (v: string) => void,
  hint?: string | null,
) {
  // Past four options the row overflows a phone, so Primer collapses it to a
  // dropdown there (its onSelect still fires each Button's onClick).
  const variant = options.length > 4 ? ({ narrow: 'dropdown' } as const) : undefined;
  return fc(label, (
    <SegmentedControl aria-label={label} size="small" variant={variant}>
      {options.map(([v, text]) => (
        <SegmentedControl.Button key={v || 'default'} selected={value === v} onClick={() => onChange(v)}>
          {text}
        </SegmentedControl.Button>
      ))}
    </SegmentedControl>
  ), hint);
}
