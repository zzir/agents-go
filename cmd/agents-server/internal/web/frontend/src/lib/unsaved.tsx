import { createContext } from 'react';

// UnsavedRegistry is a surface's view of the forms inside it holding unsaved
// edits: each reports under its own id, and the surface's close paths ask before
// discarding (invariant 41; useUnsavedRegistry provides one). Absent, nothing asks.
export interface UnsavedRegistry {
  set(id: string, dirty: boolean): void;
  any(): boolean;
}

export const UnsavedContext = createContext<UnsavedRegistry | null>(null);

// FormDirtyContext is one form's own flag, for the Cancel inside it.
export const FormDirtyContext = createContext(false);

export const DISCARD_PROMPT = {
  title: 'Discard unsaved changes?',
  content: 'The form has edits that were not saved.',
  confirmButtonContent: 'Discard',
  confirmButtonType: 'danger' as const,
};
