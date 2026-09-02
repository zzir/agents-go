import { useMemo, useState, type CSSProperties, type ReactNode } from 'react';
import { ActionList, ActionMenu, IconButton, TextInput } from '@primer/react';
import { Blankslate, DataTable, Table, type Column, type UniqueRow } from '@primer/react/experimental';
import { KebabHorizontalIcon, SearchIcon } from '@primer/octicons-react';
import { PAGE_SIZE, usePage } from '@/lib/hooks';

interface ListTableProps<T extends UniqueRow> {
  // Labels the table; the element with this id is the page's heading.
  labelledBy: string;
  rows: T[];
  // A column's `minWidth` (px) floors its track; their sum floors the table,
  // which then scrolls sideways inside Primer's ScrollableRegion instead of
  // squeezing the headers into each other.
  columns: Column<T>[];
  // Client-side search over the rows; the page resets with the query.
  search?: { placeholder: string; match: (row: T, q: string) => boolean };
  // What stands in for the table while there is nothing at all.
  empty: ReactNode;
  loading?: boolean;
  // Rendered under the pager (a "load older" for cursor-fed lists).
  footer?: ReactNode;
}

// The floor for a table whose columns declare no minimum: below it 4–6
// nowrap columns collapse onto each other.
const DEFAULT_MIN_WIDTH = 560;

function tableMinWidth<T extends UniqueRow>(columns: Column<T>[]): number {
  let sum = 0;
  for (const c of columns) {
    const w = typeof c.minWidth === 'number' ? c.minWidth : parseFloat(String(c.minWidth ?? ''));
    if (Number.isFinite(w)) sum += w;
  }
  return Math.max(sum, DEFAULT_MIN_WIDTH);
}

// ListTable is the settings pages' list: a search box in the container's
// filter slot, a DataTable over the matching rows, a pager past one page.
export function ListTable<T extends UniqueRow>({ labelledBy, rows, columns, search, empty, loading, footer }: ListTableProps<T>) {
  const [query, setQuery] = useState('');
  const q = query.trim().toLowerCase();
  const filtered = useMemo(() => q && search ? rows.filter(r => search.match(r, q)) : rows, [rows, q, search]);
  const page = usePage(filtered, PAGE_SIZE);
  const minWidth = useMemo(() => tableMinWidth(columns), [columns]);

  let body: ReactNode;
  if (loading && rows.length === 0) {
    body = <Table.Skeleton aria-labelledby={labelledBy} columns={columns} rows={4} />;
  } else if (rows.length === 0) {
    body = <div className="list-table-empty">{empty}</div>;
  } else if (filtered.length === 0) {
    body = (
      <div className="list-table-empty">
        <Blankslate><Blankslate.Description>Nothing matches “{query.trim()}”.</Blankslate.Description></Blankslate>
      </div>
    );
  } else {
    body = <DataTable aria-labelledby={labelledBy} data={page.items} columns={columns} />;
  }

  return (
    <Table.Container className="list-table" style={{ '--list-table-min': `${minWidth}px` } as CSSProperties}>
      {search && rows.length > 0 && (
        <div className="list-table-filter">
          <TextInput
            leadingVisual={SearchIcon} size="small" block
            aria-label={search.placeholder} placeholder={search.placeholder}
            value={query} onChange={e => { setQuery(e.target.value); page.setIndex(0); }}
          />
        </div>
      )}
      {body}
      {filtered.length > PAGE_SIZE && (
        // Keyed on the query: the pager is uncontrolled, and a new search
        // starts on its first page.
        <Table.Pagination key={q} aria-label="Pages" pageSize={PAGE_SIZE} totalCount={filtered.length}
          defaultPageIndex={page.index} onChange={({ pageIndex }) => page.setIndex(pageIndex)} />
      )}
      {footer}
    </Table.Container>
  );
}

// RowMenu is a row's "…" — the actions column's one control, holding the
// ActionList items the caller passes.
export function RowMenu({ label, children }: { label: string; children: ReactNode }) {
  return (
    <ActionMenu>
      <ActionMenu.Anchor>
        <IconButton icon={KebabHorizontalIcon} variant="invisible" size="small" aria-label={label} />
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="small" align="end">
        <ActionList>{children}</ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}

// The trailing actions column: unlabeled, sized to its one button.
export function actionsColumn<T extends UniqueRow>(render: (row: T) => ReactNode): Column<T> {
  return { header: '', id: 'actions', width: 'auto', align: 'end', renderCell: render };
}
