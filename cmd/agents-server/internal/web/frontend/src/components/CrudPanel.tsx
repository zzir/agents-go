import { useState, type ReactNode } from 'react';
import { Button, Label, PageHeader, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { useReadOnly, type ScopedRow } from '@/lib/access';
import { BADGE } from '@/lib/badges';
import { toast } from '@/lib/toast';

/** The list-or-form scaffold every settings panel shares: a PageHeader whose
 * "+ Add" hides while a form shows, the form in the list's place, and a
 * Blankslate when the list is empty. The form and each row stay the panel's
 * own. as="section" nests it inside a page (the Settings routes block).
 * Read-only (a member's dialog, or a scoped row not the caller's to edit):
 * no Add, and the form opens disabled — a view of the record — with Back
 * where Cancel would be, plus Delete when onDelete allows it (the admin's
 * one write on a foreign private row). */
export function CrudPanel({ title, as, description, onAdd, onCancel, onDelete, form, isEmpty, empty, children }: {
  title: string;
  as?: 'page' | 'section';
  description?: ReactNode;
  onAdd: () => void;
  // Closes the form; read-only mode's Back button, since the form's own
  // actions are disabled with the rest of it.
  onCancel?: () => void;
  // The read-only form's Delete — pass only when the caller may delete the
  // row it shows (delete is allowed where edit is not).
  onDelete?: (() => void) | null;
  form: ReactNode | null;
  isEmpty: boolean;
  empty: ReactNode;
  children: ReactNode;
}) {
  const readOnly = useReadOnly();
  const body = (
    <>
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as={as === 'section' ? 'h3' : undefined}>{title}</PageHeader.Title>
        </PageHeader.TitleArea>
        {!form && !readOnly && <PageHeader.Actions><Button onClick={onAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
        {form && readOnly && onCancel && <PageHeader.Actions>
          {onDelete && <Button onClick={onDelete} variant="danger" size="small">Delete</Button>}
          <Button onClick={onCancel} size="small">Back</Button>
        </PageHeader.Actions>}
        {description && <PageHeader.Description>{description}</PageHeader.Description>}
      </PageHeader>
      {form && (readOnly
        ? <fieldset disabled className="readonly-form settings-form">{form}</fieldset>
        : <div className="settings-form">{form}</div>)}
      {!form && (
        <div className="Box">
          {children}
          {isEmpty && <Blankslate><Blankslate.Description>{empty}</Blankslate.Description></Blankslate>}
        </div>
      )}
    </>
  );
  return as === 'section' ? <div className="form-group">{body}</div> : <Stack gap="normal">{body}</Stack>;
}

/** A row's "Edit" — "View" when the form it opens is a disabled view (the
 * dialog is read-only, or `readOnly` says this row is not the caller's). */
export function RowEditButton({ onClick, readOnly }: { onClick: () => void; readOnly?: boolean }) {
  const ctx = useReadOnly();
  return <Button onClick={onClick} size="small" variant="invisible">{(readOnly ?? ctx) ? 'View' : 'Edit'}</Button>;
}

/** A scoped row's visibility badge: "Global" for everyone, "Private" only for
 * a foreign row (the admin's cross-user view); an own private row is silent. */
export function ScopeBadge({ row, meId }: { row: ScopedRow; meId?: string }) {
  if (row.scope === 'global') return <Label variant={BADGE.scope}>Global</Label>;
  if (meId && row.owner_id && row.owner_id !== meId) return <Label variant={BADGE.scope}>Private</Label>;
  return null;
}

/** The admin's promote/demote row action: POST /<entity>/:id/scope, with the
 * server's 400/409 (non-global references, name collisions) as toasts. */
export function ScopeButton({ row, setScope, onDone }: {
  row: ScopedRow & { id: string | number };
  setScope: (id: string | number, scope: 'global' | 'private') => Promise<null>;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const global = row.scope === 'global';
  const flip = async () => {
    setBusy(true);
    try {
      await setScope(row.id, global ? 'private' : 'global');
      onDone();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return <Button onClick={flip} size="small" variant="invisible" disabled={busy}>{global ? 'Make private' : 'Make global'}</Button>;
}
