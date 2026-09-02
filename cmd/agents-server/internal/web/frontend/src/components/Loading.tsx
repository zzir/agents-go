import { Spinner } from '@primer/react';
import { SkeletonText } from '@primer/react/experimental';

/** The one loading state every surface shows: `list` stands in for rows not
 * yet fetched, `panel` centers a spinner in a body, `inline` is a muted line
 * where text will be. */
export function Loading({ kind }: { kind: 'list' | 'panel' | 'inline' }) {
  if (kind === 'list') {
    return (
      <div className="loading-list" role="status" aria-label="Loading">
        <SkeletonText lines={3} />
      </div>
    );
  }
  if (kind === 'panel') {
    return <div className="loading-panel"><Spinner size="small" srText="Loading" /></div>;
  }
  return <div className="loading-inline" role="status">Loading…</div>;
}
