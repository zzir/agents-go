import { type ReactNode } from 'react';
import { IconButton, CounterLabel } from '@primer/react';
import { XIcon } from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { useNarrow, useResizablePane } from '@/lib/hooks';

const DEFAULT_WIDTH = 440;
const MIN_WIDTH = 280;
const MAX_WIDTH = 960;

interface SidePanelProps {
  icon: Icon;
  title: string;
  count?: number;
  onClose: () => void;
  children: ReactNode;
  /** localStorage key for the persisted width — give each panel type (trace, diff, ...) its own. */
  storageKey: string;
  defaultWidth?: number;
  minWidth?: number;
  maxWidth?: number;
}

/**
 * Generic resizable right-docked panel shell: icon/title/count header, close
 * button, scrollable body, drag-to-resize handle (mirrors the sidebar's).
 * Feed it different content (trace runs today, a generated image or diff
 * later) to open a detail view without rebuilding the drawer chrome each time.
 */
export function SidePanel({ icon: PanelIcon, title, count, onClose, children, storageKey, defaultWidth = DEFAULT_WIDTH, minWidth = MIN_WIDTH, maxWidth = MAX_WIDTH }: SidePanelProps) {
  const narrow = useNarrow();
  const { width, handleProps } = useResizablePane({ storageKey, min: minWidth, max: maxWidth, defaultWidth, edge: 'right' });

  return (
    <div className="side-panel" style={narrow ? undefined : { width }}>
      {!narrow && (
        <div
          className="side-panel-handle"
          role="slider"
          aria-orientation="horizontal"
          aria-label={`Resize ${title} panel`}
          aria-valuemin={minWidth}
          aria-valuemax={maxWidth}
          aria-valuenow={width}
          aria-valuetext={`${title} panel width ${width} pixels`}
          tabIndex={0}
          {...handleProps}
        />
      )}
      <div className="side-panel-header">
        <div className="side-panel-title">
          <PanelIcon size={16} />
          <span>{title}</span>
          {count !== undefined && <CounterLabel>{count}</CounterLabel>}
        </div>
        <IconButton icon={XIcon} variant="invisible" aria-label="Close" onClick={onClose} />
      </div>
      <div className="side-panel-body">
        {children}
      </div>
    </div>
  );
}

export default SidePanel;
