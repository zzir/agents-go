import { Component, type ReactNode } from 'react';
import { Button, Flash } from '@primer/react';

/** Catches a render crash below it so the rest of the app stays alive — the
 * transcript renders arbitrary model output, and one bad payload must not
 * unmount the socket and sidebar with it. A changed resetKey (switching
 * session or tab) clears the error and tries again. */
export class ErrorBoundary extends Component<
  { children: ReactNode; resetKey?: unknown; fallback?: (retry: () => void, error: Error) => ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidUpdate(prev: { resetKey?: unknown }) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) this.setState({ error: null });
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    const retry = () => this.setState({ error: null });
    if (this.props.fallback) return this.props.fallback(retry, error);
    return (
      <Flash variant="danger" style={{ margin: 'var(--base-size-16)' }}>
        This view failed to render: {String(error.message || error)}{' '}
        <Button size="small" onClick={retry}>Try again</Button>
      </Flash>
    );
  }
}
