import { createContext, useContext } from 'react';

// ReadOnlyContext is true inside a settings dialog opened by a member: the
// server refuses them every write, so the panels show what is configured and
// offer nothing that would be refused. Scoped panels (agents, providers, MCP,
// skills, workflows) gate per row with canEditRow instead.
export const ReadOnlyContext = createContext(false);

export function useReadOnly(): boolean {
  return useContext(ReadOnlyContext);
}

// A row of the five scoped entities: scope decides who sees it, owner_id
// names its creator — permanent, kept across scope flips.
export interface ScopedRow {
  scope?: string;
  owner_id?: string;
}

// Whether the caller may edit this scoped row: the owner edits what they
// created (private or published); an admin additionally edits any global row
// — but NOT another user's private row.
export function canEditRow(isAdmin: boolean, meId: string | undefined, row: ScopedRow): boolean {
  if (!!meId && row.owner_id === meId) return true;
  return isAdmin && row.scope === 'global';
}

// Delete additionally lets an admin remove any row, foreign private included.
export function canDeleteRow(isAdmin: boolean, meId: string | undefined, row: ScopedRow): boolean {
  return isAdmin || canEditRow(isAdmin, meId, row);
}

// Whether the caller may demote (unpublish) this row: the admin, or its
// author. Promotion is admin-only.
export function canDemoteRow(isAdmin: boolean, meId: string | undefined, row: ScopedRow): boolean {
  return isAdmin || (!!meId && row.owner_id === meId);
}

// Whether `holder` (the config being edited) may REFERENCE `row` — the
// picker-side mirror of the server's RefVisible: a global holder only global
// rows, a private holder global rows plus its owner's own. Pickers filter
// with this so an admin's all-rows listing never offers a reference the save
// would refuse.
export function canReference(holder: ScopedRow, row: ScopedRow): boolean {
  if (holder.scope === 'global') return row.scope === 'global';
  return row.scope === 'global' || (!!row.owner_id && row.owner_id === holder.owner_id);
}
