import { IconButton, TextInput } from '@primer/react';
import { PlusIcon, TrashIcon } from '@primer/octicons-react';
import type { ReactElement } from 'react';
import { SECRET_MASK, type EnvVar } from '@/lib/binding';
import '@/components/env-editor.css';

/* The environment editor shared by the new-project dialog and a project's own
   environment dialog: one name/value row per variable.

   Values are write-only. What you type here is visible until it is saved and
   never again — the server masks every value on the way out, as it does for
   every other credential it stores. A masked value sent back unchanged keeps
   what is stored, so one variable can be rewritten without retyping its
   neighbours. Nothing here hides a value from the agent: it reads the
   container's environment with one command. */

interface EnvEditorProps {
  vars: EnvVar[];
  onChange: (vars: EnvVar[]) => void;
  disabled?: boolean;
}

/* Names a shell can export — the server's rule, mirrored so the complaint
   arrives while typing rather than on save. */
const NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function envError(vars: EnvVar[]): string | null {
  const seen = new Set<string>();
  for (const v of vars) {
    if (!v.key) continue;
    if (!NAME_RE.test(v.key)) return `\u201c${v.key}\u201d is not a usable variable name`;
    if (seen.has(v.key)) return `\u201c${v.key}\u201d is set twice`;
    seen.add(v.key);
  }
  return null;
}

/* Drops the blank rows an editor accumulates. */
export function cleanEnv(vars: EnvVar[]): EnvVar[] {
  return vars.filter(v => v.key.trim() !== '');
}

export function EnvEditor({ vars, onChange, disabled }: EnvEditorProps): ReactElement {
  const set = (i: number, patch: Partial<EnvVar>) =>
    onChange(vars.map((v, n) => (n === i ? { ...v, ...patch } : v)));

  return (
    <div className="env-editor">
      {vars.map((v, i) => (
        <div className="env-editor-row" key={i}>
          <TextInput
            aria-label="Name"
            placeholder="NAME"
            className="env-editor-key"
            value={v.key}
            disabled={disabled}
            onChange={e => set(i, { key: e.target.value })}
          />
          <TextInput
            aria-label="Value"
            placeholder={v.value === SECRET_MASK ? 'unchanged' : 'value'}
            className="env-editor-value"
            value={v.value}
            disabled={disabled}
            /* A stored value arrives as the mask. Select it rather than clear
               it: typing replaces the sentinel, and clicking in without
               typing leaves the stored value alone — clearing here would wipe
               it on the next save. */
            onFocus={e => { if (v.value === SECRET_MASK) e.currentTarget.select(); }}
            onChange={e => set(i, { value: e.target.value })}
          />
          <IconButton
            icon={TrashIcon}
            variant="invisible"
            size="small"
            aria-label={`Remove ${v.key || 'variable'}`}
            disabled={disabled}
            onClick={() => onChange(vars.filter((_, n) => n !== i))}
          />
        </div>
      ))}
      <div>
        <IconButton
          icon={PlusIcon}
          variant="invisible"
          size="small"
          aria-label="Add variable"
          disabled={disabled}
          onClick={() => onChange([...vars, { key: '', value: '' }])}
        />
      </div>
      <p className="env-editor-hint">
        Optional. Set on the container, so commands, shells and terminals all see them.
        <strong> Values are not shown again after saving</strong> — you can overwrite one, not read
        it back. Anything running in the container can read them all.
      </p>
    </div>
  );
}
