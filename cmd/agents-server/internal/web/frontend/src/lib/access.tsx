import { createContext, useContext } from 'react';

// ReadOnlyContext is true inside a settings dialog opened by a member: the
// server refuses them every write, so the panels show what is configured and
// offer nothing that would be refused.
export const ReadOnlyContext = createContext(false);

export function useReadOnly(): boolean {
  return useContext(ReadOnlyContext);
}
