import { Label } from '@primer/react';

// The task/run status vocabulary every surface shows — one color table and
// one wording, whether it renders as a dot, a Label, or the wf-outcome chip.

const STATUS_VARIANT: Record<string, 'accent' | 'attention' | 'success' | 'danger' | 'secondary'> = {
  working: 'accent', input_required: 'attention', completed: 'success', failed: 'danger', cancelled: 'secondary',
};

export function statusVariant(status: string): 'accent' | 'attention' | 'success' | 'danger' | 'secondary' {
  return STATUS_VARIANT[status] || 'secondary';
}

export function statusText(status: string): string {
  return status.replace('_', ' ');
}

export function isLive(status: string): boolean {
  return status === 'working' || status === 'input_required';
}

/** The status as the colored dot every list shows beside its title — the
 * words live in the tooltip/aria-label. Live states pulse (chat.css). */
export function statusDot(status: string) {
  const text = statusText(status);
  return <span className={'task-status-dot task-status-dot-' + status} title={text} aria-label={text} role="img" />;
}

/** The status as a Primer Label, for table/card contexts where the words show. */
export function StatusLabel({ status, prefix, size }: { status: string; prefix?: string; size?: 'small' | 'large' }) {
  return <Label variant={statusVariant(status)} size={size}>{(prefix ? prefix + ' ' : '') + statusText(status)}</Label>;
}
