import { useCallback, useState, type ReactNode } from 'react';
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

// OwnerDialog picks the account something moves to; the caller words it.
export function OwnerDialog({ title, action, caption, failure, users, move, onClose, onDone }: {
  title: string;
  // The primary button's verb ("Transfer", "Reassign").
  action: string;
  caption: ReactNode;
  // The toast when the refusal carries no message.
  failure: string;
  users: { id: string; name?: string; email: string }[];
  move: (userId: string) => Promise<unknown>;
  onClose: () => void;
  onDone: (userId: string) => void;
}) {
  const [userId, setUserId] = useState(users[0]?.id || '');
  const [busy, setBusy] = useState(false);

  const run = useCallback(async () => {
    setBusy(true);
    try {
      await move(userId);
      onDone(userId);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : failure);
      setBusy(false);
    }
  }, [move, userId, onDone, failure]);

  return (
    <Dialog
      title={title}
      onClose={onClose}
      width="medium"
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: action, disabled: busy || !userId, onClick: () => { void run(); } },
      ]}
    >
      <Stack gap="normal">
        <FormControl required>
          <FormControl.Label>New owner</FormControl.Label>
          <FormControl.Caption>{caption}</FormControl.Caption>
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
  // One more sentence for the caption, e.g. what moves along with the row.
  note?: (row: TransferRow) => string | undefined;
}) {
  const [row, setRow] = useState<TransferRow | null>(null);
  const { me } = useMe();
  const { users, labelFor } = useOwnerLabels();
  // Scope stays put: a published row stays published, under its new author.
  const extra = row ? note?.(row) : undefined;
  const dialog = row ? (
    <OwnerDialog
      title={`Transfer ${kindLabel.replace(/s$/, '').toLowerCase()}`}
      action="Transfer"
      failure="Failed to transfer."
      caption={<>
        &quot;{row.name}&quot; ({row.owner_id ? labelFor(row.owner_id) : 'no author'}) becomes theirs to edit — any credential on it
        included. It stays {row.scope === 'global' ? 'published to the team' : 'private'}.
        {extra ? ' ' + extra : ''}
        {row.owner_id === me?.id ? ' You will no longer be its author.' : ''}
      </>}
      users={users.filter(u => u.id !== LOCAL_USER_ID && u.id !== row.owner_id)}
      move={userId => setOwner(String(row.id), userId)}
      onClose={() => setRow(null)}
      onDone={() => { setRow(null); onDone(); }}
    />
  ) : null;
  return { start: (r: TransferRow) => setRow(r), dialog };
}
