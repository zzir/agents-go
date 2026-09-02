import { useCallback, useState } from 'react';
import { Dialog, FormControl, Select, Stack } from '@primer/react';
import { useMe } from '@/lib/me';
import { LOCAL_USER_ID, useOwnerLabels } from '@/lib/owners';
import { toast } from '@/lib/toast';

export interface TransferRow {
  id: string | number;
  name: string;
  scope?: string;
  owner_id?: string;
}

// TransferDialog picks the account a scoped row moves to. Scope stays put: a
// published row stays published, under its new author.
export function TransferDialog({ row, kindLabel, owner, users, transfer, onClose, onDone, meId, note }: {
  row: TransferRow;
  kindLabel: string;
  owner: string;
  users: { id: string; name?: string; email: string }[];
  transfer: (userId: string) => Promise<null>;
  onClose: () => void;
  onDone: () => void;
  meId?: string;
  // One more sentence for the caption, e.g. what moves along with the row.
  note?: string;
}) {
  const [userId, setUserId] = useState(users[0]?.id || '');
  const [busy, setBusy] = useState(false);

  const run = useCallback(async () => {
    setBusy(true);
    try {
      await transfer(userId);
      onDone();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : 'Failed to transfer.');
      setBusy(false);
    }
  }, [transfer, userId, onDone]);

  return (
    <Dialog
      title={`Transfer ${kindLabel.replace(/s$/, '').toLowerCase()}`}
      onClose={onClose}
      width="medium"
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: 'Transfer', disabled: busy || !userId, onClick: () => { void run(); } },
      ]}
    >
      <Stack gap="normal">
        <FormControl required>
          <FormControl.Label>New owner</FormControl.Label>
          <FormControl.Caption>
            &quot;{row.name}&quot; ({owner}) becomes theirs to edit — any credential on it
            included. It stays {row.scope === 'global' ? 'published to the team' : 'private'}.
            {note ? ' ' + note : ''}
            {row.owner_id === meId ? ' You will no longer be its author.' : ''}
          </FormControl.Caption>
          <Select block value={userId} onChange={e => setUserId(e.target.value)}>
            {users.length === 0 ? <Select.Option value="">No other account</Select.Option> : null}
            {users.map(u => <Select.Option key={u.id} value={u.id}>{u.name ? `${u.name} (${u.email})` : u.email}</Select.Option>)}
          </Select>
        </FormControl>
      </Stack>
    </Dialog>
  );
}

// useTransfer holds one panel's transfer state: `start(row)` opens the
// dialog, `dialog` is what the panel renders beside its list.
export function useTransfer({ kindLabel, setOwner, onDone, note }: {
  kindLabel: string;
  setOwner: (id: string, userId: string) => Promise<null>;
  onDone: () => void;
  note?: (row: TransferRow) => string | undefined;
}) {
  const [row, setRow] = useState<TransferRow | null>(null);
  const { me } = useMe();
  const { users, labelFor } = useOwnerLabels();
  const dialog = row ? (
    <TransferDialog
      row={row}
      kindLabel={kindLabel}
      owner={row.owner_id ? labelFor(row.owner_id) : 'no author'}
      users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== row.owner_id)}
      transfer={userId => setOwner(String(row.id), userId)}
      onClose={() => setRow(null)}
      onDone={() => { setRow(null); onDone(); }}
      meId={me?.id}
      note={note?.(row)}
    />
  ) : null;
  return { start: (r: TransferRow) => setRow(r), dialog };
}
