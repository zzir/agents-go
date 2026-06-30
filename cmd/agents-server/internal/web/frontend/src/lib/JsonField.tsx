import { useState, type ChangeEvent } from 'react';
import { FormControl, TextInput, Textarea } from '@primer/react';

interface JsonFieldProps {
  label: string;
  value: string;
  onChange: (value: string) => void;
  caption?: string;
  placeholder?: string;
  multiline?: boolean;
  rows?: number;
}

/**
 * A JSON-valued form field: a monospace input that validates on blur and shows
 * a Primer error state for malformed JSON, so bad JSON can't silently reach the
 * backend. Empty is treated as valid — the caller decides what empty means.
 */
export function JsonField({ label, value, onChange, caption, placeholder, multiline, rows = 3 }: JsonFieldProps) {
  const [error, setError] = useState<string | null>(null);

  const validate = (v: string) => {
    if (!v.trim()) { setError(null); return; }
    try { JSON.parse(v); setError(null); }
    catch (e) { setError((e as Error).message); }
  };

  const shared = {
    value,
    onChange: (e: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      onChange(e.target.value);
      if (error) setError(null);
    },
    onBlur: () => validate(value),
    placeholder,
    block: true,
    validationStatus: error ? ('error' as const) : undefined,
  };

  return (
    <FormControl>
      <FormControl.Label>{label}</FormControl.Label>
      {multiline
        ? <Textarea rows={rows} style={{ fontFamily: 'var(--fontStack-monospace)' }} {...shared} />
        : <TextInput monospace {...shared} />}
      {error
        ? <FormControl.Validation variant="error">Invalid JSON: {error}</FormControl.Validation>
        : caption ? <FormControl.Caption>{caption}</FormControl.Caption> : null}
    </FormControl>
  );
}
