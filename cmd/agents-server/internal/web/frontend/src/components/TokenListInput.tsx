import { useState } from 'react';
import { TextInputWithTokens } from '@primer/react';

/** A list-valued field as removable tokens. Enter or comma commits what is
 * typed; blur commits too, so a half-typed entry is not silently lost. The
 * caller keeps its own storage format — this only speaks string[]. */
export function TokenListInput({ values, onChange, placeholder, ariaLabel }: {
  values: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  ariaLabel: string;
}) {
  const [draft, setDraft] = useState('');
  const commit = () => {
    const t = draft.trim();
    if (!t) return;
    if (!values.includes(t)) onChange([...values, t]);
    setDraft('');
  };
  return (
    <TextInputWithTokens
      block
      aria-label={ariaLabel}
      tokens={values.map((text, id) => ({ id, text }))}
      onTokenRemove={id => onChange(values.filter((_, i) => i !== id))}
      value={draft}
      onChange={e => setDraft(e.target.value)}
      onKeyDown={e => { if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); commit(); } }}
      onBlur={commit}
      placeholder={values.length === 0 ? placeholder : undefined}
    />
  );
}
