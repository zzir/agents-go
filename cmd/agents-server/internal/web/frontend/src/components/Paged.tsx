import { type ReactNode } from 'react';
import { Table } from '@primer/react/experimental';
import { PAGE_SIZE, type usePage } from '@/lib/hooks';

/** Wraps a client-paged list: the hub-paged frame and the pager render only
 * when there is more than one page. `total` is the unpaged row count. */
export function Paged<T>({ page, total, label, children }: {
  page: ReturnType<typeof usePage<T>>;
  total: number;
  label: string;
  children: ReactNode;
}) {
  return (
    <div className={page.count > 1 ? 'hub-paged' : undefined}>
      {children}
      {page.count > 1 && (
        <Table.Pagination aria-label={label} pageSize={PAGE_SIZE} totalCount={total}
          defaultPageIndex={page.index} onChange={({ pageIndex }) => page.setIndex(pageIndex)} />
      )}
    </div>
  );
}
