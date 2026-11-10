import { type ReactNode } from 'react';
import { IconButton, CounterLabel } from '@primer/react';
import { XIcon } from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { useNarrow, useResizablePane } from '@/lib/hooks';

// One width for every Inspector lens: switching lenses keeps the size.
const WIDTH_KEY = 'inspectorWidth';
const DEFAULT_WIDTH = 440;
const MIN_WIDTH = 280;
const MAX_WIDTH = 960;

interface SidePanelProps {
  icon: Icon;
  title: string;
  /** Header badge: a count for list panels, a formatted value ("37%") where a bare number would read as one. */
  count?: number | string;
  onClose: () => void;
  children: ReactNode;
}

/** Generic resizable right-docked panel shell: icon/title/count header, close
 * button, scrollable body, drag-to-resize handle (mirrors the sidebar's). Any
 * detail view (trace runs, an image, a diff) opens in it. */
export function SidePanel({ icon: PanelIcon, title, count, onClose, children }: SidePanelProps) {
  const narrow = useNarrow();
  const { width, dragging, handleProps } = useResizablePane({ storageKey: WIDTH_KEY, min: MIN_WIDTH, max: MAX_WIDTH, defaultWidth: DEFAULT_WIDTH, edge: 'right' });

  return (
    <div className="side-panel" style={narrow ? undefined : { width }}>
      {!narrow && (
        <div
          className={'side-panel-handle pane-resize-handle' + (dragging ? ' dragging' : '')}
          role="slider"
          aria-orientation="horizontal"
          aria-label={`Resize ${title} panel`}
          aria-valuemin={MIN_WIDTH}
          aria-valuemax={MAX_WIDTH}
          aria-valuenow={width}
          aria-valuetext={`${title} panel width ${width} pixels`}
          tabIndex={0}
          {...handleProps}
        />
      )}
      <div className="side-panel-header">
        <div className="side-panel-title">
          <PanelIcon size={16} />
          <span className="side-panel-title-text" title={title}>{title}</span>
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
