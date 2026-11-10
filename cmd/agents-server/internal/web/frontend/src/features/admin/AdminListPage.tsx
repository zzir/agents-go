import type { ReactNode } from 'react';
import { PageHeader, Stack } from '@primer/react';
import { Blankslate, type Column, type UniqueRow } from '@primer/react/experimental';
import type { Icon } from '@primer/octicons-react';
import { ListTable } from '@/components/ListTable';

// AdminListPage is an admin tab that is one searchable table: its heading and
// one-line description, the rows, and whatever dialogs the tab opens.
export function AdminListPage<T extends UniqueRow>({ titleId, title, description, what, rows, error, onRetry, columns, match, empty, children }: {
  // The heading's id, which labels the table.
  titleId: string;
  title: string;
  description: ReactNode;
  // The list's plural noun: the search placeholder and a failed read's line.
  what: string;
  // null while the first read is out.
  rows: T[] | null;
  error: string | null;
  onRetry: () => void;
  columns: Column<T>[];
  match: (row: T, q: string) => boolean;
  empty: { icon: Icon; heading: string; text: ReactNode };
  children?: ReactNode;
}) {
  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title><span id={titleId}>{title}</span></PageHeader.Title>
        </PageHeader.TitleArea>
        <PageHeader.Description>{description}</PageHeader.Description>
      </PageHeader>
      <ListTable
        labelledBy={titleId}
        rows={rows ?? []}
        columns={columns}
        loading={rows === null}
        what={what}
        error={error}
        onRetry={onRetry}
        search={{ placeholder: `Search ${what}`, match }}
        empty={(
          <Blankslate>
            <Blankslate.Visual><empty.icon size={24} /></Blankslate.Visual>
            <Blankslate.Heading>{empty.heading}</Blankslate.Heading>
            <Blankslate.Description>{empty.text}</Blankslate.Description>
          </Blankslate>
        )}
      />
      {children}
    </Stack>
  );
}
