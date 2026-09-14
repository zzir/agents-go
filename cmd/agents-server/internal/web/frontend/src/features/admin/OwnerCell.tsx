import { OwnerName, type UserLabel } from '@/lib/owners';
import './admin.css';

// OwnerCell names a row's author the way every admin table does; a row with
// no author (a global row from before ownership was stamped) says so quietly
// rather than showing an unknown person.
export function OwnerCell({ ownerId, ownerOf, labelFor }: {
  ownerId?: string;
  ownerOf: (id?: string) => UserLabel | undefined;
  labelFor: (id?: string) => string;
}) {
  if (!ownerId) return <span className="list-clip owner-none">no author</span>;
  const owner = ownerOf(ownerId);
  // Name and email both, as the Members table shows a person: members can
  // share a name, and the email is the identity accounts merge on.
  if (owner?.name && owner.email) {
    return (
      <span className="list-person-text">
        <span className="list-clip">{owner.name}</span>
        <span className="list-clip account-muted">{owner.email}</span>
      </span>
    );
  }
  return <OwnerName owner={owner} fallback={labelFor(ownerId)} />;
}

// ownerLabel is the text form of the same, for search and confirmations.
export const ownerLabel = (labelFor: (id?: string) => string, ownerId?: string) =>
  ownerId ? labelFor(ownerId) : 'no author';
