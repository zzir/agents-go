import { type ReactNode } from 'react';

/** One entity row in a settings list: dot + title + badges on the head line,
 * an optional sub or meta line under it, actions held to the right edge. */
export function ResourceRow({ status, title, badges, sub, meta, actions }: {
  status?: ReactNode;
  title: ReactNode;
  badges?: ReactNode;
  sub?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="Box-row">
      <div className="resource-row-main">
        <div className="resource-row-head">
          {status}
          <span className="resource-row-title">{title}</span>
          {badges}
        </div>
        {sub ? <div className="resource-row-sub">{sub}</div> : null}
        {meta ? <div className="resource-row-meta">{meta}</div> : null}
      </div>
      {actions ? <div className="resource-row-actions">{actions}</div> : null}
    </div>
  );
}
