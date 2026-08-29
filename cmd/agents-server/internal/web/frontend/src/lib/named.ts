/** A row another entity points at by id — shown by name, or an 8-char id stub.
 * Agent rows also carry their avatar path, rendered where the name is. */
export interface Named { id: string; name?: string; avatar?: string }

export const nameOf = (list: Named[] | null | undefined, id: string): string =>
  (list || []).find(x => x.id === id)?.name || id.slice(0, 8);
