import { Tooltip } from '@primer/react';
import { api } from '@/lib/api';
import { useApi, invalidate } from '@/lib/hooks';
import { useIsAdmin } from '@/lib/me';

// The token-mode admin: never a transfer target, never listed as a member.
export const LOCAL_USER_ID = '00000000-0000-0000-0000-000000000001';

export interface UserLabel {
  id: string;
  name?: string;
  email: string;
}

// The directory is one small, slow-moving list every scoped panel needs: one
// cache entry, shared by every mount.
const DIRECTORY_KEY = 'user-labels';
const NO_USERS: UserLabel[] = [];

/** Drops the cached directory (a member was added or renamed); every mounted
 * useOwnerLabels refetches. */
export function reloadDirectory(): void {
  invalidate(DIRECTORY_KEY);
}

// useOwnerLabels serves the id→person directory row owners render from. The
// server gives it to admins only, so a member never asks and `labelFor` shows
// a short id.
export function useOwnerLabels() {
  const admin = useIsAdmin() === true;
  const { data } = useApi<UserLabel[]>(
    () => (admin ? api.auth.userLabels() : Promise.resolve(NO_USERS)),
    [admin], admin ? DIRECTORY_KEY : undefined);
  const users = data ?? NO_USERS;
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
