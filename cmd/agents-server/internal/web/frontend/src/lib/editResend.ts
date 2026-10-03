// Editing a sent message: the bubble turns into a box, and only sending
// branches the session — at the message's parent — and runs the new text
// (spec §2.5d); clicking Edit touches nothing on the server.

export interface EditableMessage {
  entryId?: string;
  parentId?: string;
  content: string;
}

// canEditMessage: a stored message that is not the first, while no run is
// live and no decision is pending (a branch would abandon the pause).
export function canEditMessage(m: EditableMessage, state: { running: boolean; pendingDecision: boolean }): boolean {
  return !!m.entryId && !!m.parentId && !state.running && !state.pendingDecision;
}

export interface ResendDeps {
  // branch moves the session's active branch to the entry; it answers the
  // leaf it left, for a rollback.
  branch: (entryId: string) => Promise<{ previous_leaf: string }>;
  // reload re-reads the timeline after a branch move.
  reload: () => Promise<void>;
  // send starts the run with the new text; false when the socket is down.
  send: (text: string) => boolean;
}

export type ResendOutcome = 'sent' | 'rolled_back' | 'stranded';

// resendEdited branches to the message's parent and sends the edited text; a
// send that fails rolls the branch back (stranded when even that fails).
export async function resendEdited(deps: ResendDeps, parentId: string, text: string): Promise<ResendOutcome> {
  const { previous_leaf } = await deps.branch(parentId);
  await deps.reload();
  if (deps.send(text)) return 'sent';
  try {
    await deps.branch(previous_leaf);
    await deps.reload();
    return 'rolled_back';
  } catch {
    return 'stranded';
  }
}
