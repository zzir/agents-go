import { createContext, useContext } from 'react';

// ReadOnlyContext is true inside a settings dialog opened by a member: the
// server refuses them every write, so the panels show what is configured and
// offer nothing that would be refused. Scoped panels (agents, providers, MCP,
// skills, workflows) gate per row with canEditRow instead.
export const ReadOnlyContext = createContext(false);

export function useReadOnly(): boolean {
  return useContext(ReadOnlyContext);
}

// A row of the five scoped entities: global rows are shared, private rows
// carry their owner.
export interface ScopedRow {
  scope?: string;
  owner_id?: string;
}

// Whether the caller may edit this scoped row: an admin for global rows, the
// owner for private ones — an admin does NOT edit another user's private row.
export function canEditRow(isAdmin: boolean, meId: string | undefined, row: ScopedRow): boolean {
  if (row.scope === 'global') return isAdmin;
  return !!meId && row.owner_id === meId;
}

// Delete additionally lets an admin remove any row, foreign private included.
export function canDeleteRow(isAdmin: boolean, meId: string | undefined, row: ScopedRow): boolean {
  return isAdmin || canEditRow(isAdmin, meId, row);
}
