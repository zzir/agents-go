import { useEffect, useState } from 'react';
import { Tooltip } from '@primer/react';
import { api } from '@/lib/api';

// The token-mode admin: never a transfer target, never listed as a member.
export const LOCAL_USER_ID = '00000000-0000-0000-0000-000000000001';

export interface UserLabel {
  id: string;
  name?: string;
  email: string;
}

// The directory is one small, slow-moving list every scoped panel needs, so it is
// fetched once and shared; a mount refetches only past STALE_MS (or reloadDirectory).
const STALE_MS = 60_000;
let directory: UserLabel[] | null = null;
let loadedAt = 0;
let inflight: Promise<UserLabel[]> | null = null;
const listeners = new Set<(u: UserLabel[]) => void>();

function loadDirectory(): Promise<UserLabel[]> {
  inflight ??= (api.auth.userLabels() as Promise<UserLabel[]>)
    .then(list => {
      directory = list ?? [];
      loadedAt = Date.now();
      for (const notify of listeners) notify(directory);
      return directory;
    })
    .catch(() => directory ?? []) // a failed load must not poison the next try
    .finally(() => { inflight = null; });
  return inflight;
}

/** Refetches the directory now (a member was added or renamed) and updates
 * every mounted useOwnerLabels. */
export function reloadDirectory(): Promise<void> {
  loadedAt = 0;
  return loadDirectory().then(() => undefined);
}

// useOwnerLabels serves the id→person directory the scoped panels render row
// owners from (every member may read it — decisions §5.29); `labelFor` falls
// back to a short id until it arrives.
export function useOwnerLabels() {
  const [users, setUsers] = useState<UserLabel[]>(directory ?? []);
  useEffect(() => {
    listeners.add(setUsers);
    if (directory) setUsers(directory);
    if (!directory || Date.now() - loadedAt > STALE_MS) void loadDirectory();
    return () => { listeners.delete(setUsers); };
  }, []);
  const ownerOf = (ownerId?: string): UserLabel | undefined =>
    ownerId ? users.find(x => x.id === ownerId) : undefined;
  const labelFor = (ownerId?: string) => {
    if (!ownerId) return 'unknown';
    const u = ownerOf(ownerId);
    return u?.name || u?.email || ownerId.slice(0, 8);
  };
  return { users, ownerOf, labelFor };
}

// OwnerName names a person by the NAME they signed in with, the email (too long
// for a column) on hover; an account with no name shows its email once.
export function OwnerName({ owner, fallback }: { owner?: UserLabel; fallback: string }) {
  const shown = owner?.name || owner?.email || fallback;
  if (!owner?.name || !owner.email) return <span className="list-clip" title={shown}>{shown}</span>;
  return (
    <Tooltip text={owner.email} direction="n" type="description">
      <span className="list-clip">{owner.name}</span>
    </Tooltip>
  );
}
