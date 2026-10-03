import { useId, useState } from 'react';
import { TextInputWithTokens } from '@primer/react';

/** A list-valued field as removable tokens. Enter or comma commits what is
 * typed; blur commits too, so a half-typed entry is not silently lost. The
 * caller keeps its own storage format — this only speaks string[].
 * `suggestions` offers names to pick from (a datalist); any text still commits. */
export function TokenListInput({ values, onChange, placeholder, ariaLabel, suggestions }: {
  values: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  ariaLabel: string;
  suggestions?: string[];
}) {
  const [draft, setDraft] = useState('');
  const listId = useId();
  const commit = () => {
    const t = draft.trim();
    if (!t) return;
    if (!values.includes(t)) onChange([...values, t]);
    setDraft('');
  };
  return (
    <>
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
        list={suggestions?.length ? listId : undefined}
      />
      {!!suggestions?.length && (
        <datalist id={listId}>{suggestions.map(s => <option key={s} value={s} />)}</datalist>
      )}
    </>
  );
}
