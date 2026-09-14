import { createContext } from 'react';

// UnsavedRegistry is a dialog's view of the forms inside it that hold edits
// not yet saved: each form reports under its own id, and the dialog's close
// paths ask before discarding any. Absent (a form outside a dialog), nothing asks.
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
