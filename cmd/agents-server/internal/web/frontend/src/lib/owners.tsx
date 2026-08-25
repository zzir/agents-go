import { useEffect, useState } from 'react';
import { Tooltip } from '@primer/react';
import { api } from '@/lib/api';

export interface UserLabel {
  id: string;
  name?: string;
  email: string;
}

// The directory is one small, slow-moving list that every scoped panel needs,
// so it is fetched ONCE per page load and shared: a panel that refetched it on
// mount would blank its rows again the moment the answer arrived.
let directory: UserLabel[] | null = null;
let inflight: Promise<UserLabel[]> | null = null;
const waiting = new Set<(u: UserLabel[]) => void>();

function loadDirectory(): Promise<UserLabel[]> {
  inflight ??= (api.auth.userLabels() as Promise<UserLabel[]>)
    .then(list => {
      directory = list ?? [];
      for (const notify of waiting) notify(directory);
      return directory;
    })
    .catch(() => {
      inflight = null; // a failed load must not poison the next panel's try
      return [];
    });
  return inflight;
}

// useOwnerLabels serves the id→person directory the scoped panels render row
// owners from. Every member may read it (one team, one trust boundary — spec
// §5.29); `labelFor` falls back to a short id until it arrives.
export function useOwnerLabels() {
  const [users, setUsers] = useState<UserLabel[]>(directory ?? []);
  useEffect(() => {
    if (directory) return;
    let live = true;
    const notify = (list: UserLabel[]) => { if (live) setUsers(list); };
    waiting.add(notify);
    void loadDirectory();
    return () => { live = false; waiting.delete(notify); };
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

// OwnerName is how every management listing names a person: the NAME they
// signed in with, with the email — which is the identity accounts merge on,
// but too long for a column — on hover. An account with no name shows its
// email and needs no second copy of it.
export function OwnerName({ owner, fallback }: { owner?: UserLabel; fallback: string }) {
  const shown = owner?.name || owner?.email || fallback;
  if (!owner?.name || !owner.email) return <span className="list-clip" title={shown}>{shown}</span>;
  return (
    <Tooltip text={owner.email} direction="n" type="description">
      <span className="list-clip">{owner.name}</span>
    </Tooltip>
  );
}
