import { type ReactNode } from 'react';
import { Button, PageHeader, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { useReadOnly } from '@/lib/access';

/** The list-or-form scaffold every settings panel shares: a PageHeader whose
 * "+ Add" hides while a form shows, the form in the list's place, and a
 * Blankslate when the list is empty. The form and each row stay the panel's
 * own. as="section" nests it inside a page (the Settings routes block).
 * Read-only (a member's dialog): no Add, and the form opens disabled — a
 * view of the record — with Back where Cancel would be. */
export function CrudPanel({ title, as, description, onAdd, onCancel, form, isEmpty, empty, children }: {
  title: string;
  as?: 'page' | 'section';
  description?: ReactNode;
  onAdd: () => void;
  // Closes the form; read-only mode's Back button, since the form's own
  // actions are disabled with the rest of it.
  onCancel?: () => void;
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
        {form && readOnly && onCancel && <PageHeader.Actions><Button onClick={onCancel} size="small">Back</Button></PageHeader.Actions>}
        {description && <PageHeader.Description>{description}</PageHeader.Description>}
      </PageHeader>
      {form && (readOnly ? <fieldset disabled className="readonly-form">{form}</fieldset> : form)}
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

/** A row's "Edit" — "View" when the dialog is read-only, since the form it
 * opens is then a disabled view. */
export function RowEditButton({ onClick }: { onClick: () => void }) {
  const readOnly = useReadOnly();
  return <Button onClick={onClick} size="small" variant="invisible">{readOnly ? 'View' : 'Edit'}</Button>;
}
