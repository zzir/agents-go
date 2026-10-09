import { Button, Flash } from '@primer/react';
import './load-error.css';

/** A list that failed to load says so above its rows, with Retry (invariant 79).
 * `what` is the list's plural noun ("agents", "MCP servers"). */
export function LoadError({ what, error, onRetry }: { what: string; error: string; onRetry?: () => void }) {
  return (
    <Flash variant="danger" className="load-error">
      <span className="load-error-text">Could not load {what}: {error}</span>
      {onRetry && <Button size="small" onClick={onRetry}>Retry</Button>}
    </Flash>
  );
}
