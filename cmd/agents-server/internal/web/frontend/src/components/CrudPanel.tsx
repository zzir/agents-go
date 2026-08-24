import { type ReactNode } from 'react';
import { ActionList, Button, Label, PageHeader, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { RowMenu } from '@/components/ListTable';
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

/** A row's "…" overflow, the one action control a list row carries:
 * Edit ("View" when the form it opens is a disabled view — the dialog is
 * read-only, or `editReadOnly` says this row is not the caller's), a Fork
 * (open the CREATE form pre-filled from this row — nothing is written until
 * Save), the admin's scope flip, and a Delete the caller confirms. Renders
 * nothing when the caller can do none of it. */
export function RowActionsMenu({ name, onEdit, editReadOnly, onFork, scope, onDelete }: {
  name: string;
  onEdit?: () => void;
  editReadOnly?: boolean;
  // Offered on every visible row: forking a global row is how a member gets
  // an editable private copy.
  onFork?: () => void;
  // Pass only for admins: the promote/demote item. POST /<entity>/:id/scope,
  // with the server's 400/409 (non-global references, name collisions) as toasts.
  scope?: {
    row: ScopedRow & { id: string | number };
    setScope: (id: string | number, scope: 'global' | 'private') => Promise<null>;
    onDone: () => void;
  };
  // Delete where edit is not offered (the admin on a foreign private row).
  onDelete?: () => void;
}) {
  const ctx = useReadOnly();
  if (!onEdit && !onFork && !scope && !onDelete) return null;
  const global = scope?.row.scope === 'global';
  const flip = async () => {
    if (!scope) return;
    try {
      await scope.setScope(scope.row.id, global ? 'private' : 'global');
      scope.onDone();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };
  return (
    <RowMenu label={`Actions for ${name}`}>
      {onEdit && <ActionList.Item onSelect={onEdit}>{(editReadOnly ?? ctx) ? 'View' : 'Edit'}</ActionList.Item>}
      {onFork && <ActionList.Item onSelect={onFork}>Fork</ActionList.Item>}
      {scope && <ActionList.Item onSelect={() => void flip()}>{global ? 'Make private' : 'Make global'}</ActionList.Item>}
      {onDelete && <ActionList.Item variant="danger" onSelect={onDelete}>Delete</ActionList.Item>}
    </RowMenu>
  );
}

/** A scoped row's visibility badge: "Global" for everyone, "Private" only for
 * a foreign row (the admin's cross-user view); an own private row is silent. */
export function ScopeBadge({ row, meId }: { row: ScopedRow; meId?: string }) {
  if (row.scope === 'global') return <Label variant={BADGE.scope}>Global</Label>;
  if (meId && row.owner_id && row.owner_id !== meId) return <Label variant={BADGE.scope}>Private</Label>;
  return null;
}

