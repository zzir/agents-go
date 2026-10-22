import type { ReactNode } from 'react';
import { IconButton } from '@primer/react';
import { ArrowLeftIcon } from '@primer/octicons-react';

interface LensHeadProps {
  onBack: () => void;
  /** Names the list lens the arrow leads back to: "Back to tasks". */
  backLabel: string;
  /** What the list row shows before its label: the status dot, a kind mark. */
  marks?: ReactNode;
  id: string;
  /** The chips after the id (error, queued, replaced). */
  tags?: ReactNode;
  /** What the item is doing right now; gives way before the figures. */
  activity?: string;
  /** The row's right end, dot-joined: what the item took. */
  figures?: string[];
  /** After the figures: the item's buttons. */
  actions?: ReactNode;
  /** A tab row, when the lens has views. */
  children?: ReactNode;
}

/** A detail lens's head under the panel header (which stays the list's): one
 * row — back, marks, id, tags, activity on the left; figures and buttons on
 * the right. The item's label is the list row's and the transcript's, not the
 * head's. Sticky while the body scrolls — invariant 94. */
export function LensHead({ onBack, backLabel, marks, id, tags, activity, figures, actions, children }: LensHeadProps) {
  const figure = figures?.filter(Boolean).join(' · ');
  return (
    <div className="lens-chrome">
      <div className="lens-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label={backLabel} tooltipDirection="se" onClick={onBack} />
        {marks}
        <span className="lens-head-id" title={id}>{id}</span>
        {tags}
        {activity && <span className="lens-head-activity">{activity}</span>}
        <span className="lens-head-spacer" />
        {figure && <span className="lens-head-fig">{figure}</span>}
        {actions && <span className="lens-actions">{actions}</span>}
      </div>
      {children}
    </div>
  );
}
