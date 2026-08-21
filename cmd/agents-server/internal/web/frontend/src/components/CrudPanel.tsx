import { type ReactNode } from 'react';
import { Button, PageHeader, Stack } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';

/** The list-or-form scaffold every settings panel shares: a PageHeader whose
 * "+ Add" hides while a form shows, the form in the list's place, and a
 * Blankslate when the list is empty. The form and each row stay the panel's
 * own. as="section" nests it inside a page (the Settings routes block). */
export function CrudPanel({ title, as, description, onAdd, form, isEmpty, empty, children }: {
  title: string;
  as?: 'page' | 'section';
  description?: ReactNode;
  onAdd: () => void;
  form: ReactNode | null;
  isEmpty: boolean;
  empty: ReactNode;
  children: ReactNode;
}) {
  const body = (
    <>
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title as={as === 'section' ? 'h3' : undefined}>{title}</PageHeader.Title>
        </PageHeader.TitleArea>
        {!form && <PageHeader.Actions><Button onClick={onAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
        {description && <PageHeader.Description>{description}</PageHeader.Description>}
      </PageHeader>
      {form}
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
