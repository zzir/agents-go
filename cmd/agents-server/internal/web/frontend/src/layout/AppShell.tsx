import { type ReactNode, useCallback } from 'react';
import { IconButton } from '@primer/react';
import { MoonIcon, SunIcon, ThreeBarsIcon } from '@primer/octicons-react';
import { useTheme } from '@/theme/ThemeProvider';
import { useNarrow, useResizablePane } from '@/lib/hooks';
import { UserMenu } from '@/layout/UserMenu';

const PANE_WIDTH_KEY = 'paneWidth';
const PANE_MIN = 260;
const PANE_MAX = 400;
const PANE_DEFAULT = 300;

interface AppShellProps {
  onSettingsOpen: () => void;
  onAdminOpen: () => void;
  sidebarPane: ReactNode;
  sidebarOpen: boolean;
  onSidebarToggle: (open: boolean) => void;
  children: ReactNode;
}

export function AppShell({ onSettingsOpen, onAdminOpen, sidebarPane, sidebarOpen, onSidebarToggle, children }: AppShellProps) {
  const { theme, toggle } = useTheme();
  const narrow = useNarrow();
  const closeSidebar = useCallback(() => onSidebarToggle(false), [onSidebarToggle]);

  const { width, dragging, handleProps } = useResizablePane({ storageKey: PANE_WIDTH_KEY, min: PANE_MIN, max: PANE_MAX, defaultWidth: PANE_DEFAULT, edge: 'left' });

  return (
    <div className={'app-layout' + (sidebarOpen ? ' sidebar-open' : '')}>
      {narrow && (
        <header className="mobile-header">
          <IconButton icon={ThreeBarsIcon} variant="invisible" aria-label="Open sidebar" onClick={() => onSidebarToggle(true)} />
          <span className="mobile-header-title" />
          <IconButton icon={theme === 'day' ? MoonIcon : SunIcon} variant="invisible" aria-label="Toggle theme" onClick={toggle} />
          <UserMenu onSettingsOpen={onSettingsOpen} onAdminOpen={onAdminOpen} compact />
        </header>
      )}

      {narrow && <div className="sidebar-backdrop" onClick={closeSidebar} />}

      <div className="app-body">
        <div className="app-sidebar-pane" style={narrow ? undefined : { width }}>
          <div className="sidebar-container">
            <div className="sidebar-body">
              {sidebarPane}
            </div>
            {!narrow && (
              <div className="sidebar-footer">
                <UserMenu onSettingsOpen={onSettingsOpen} onAdminOpen={onAdminOpen} />
                <IconButton icon={theme === 'day' ? MoonIcon : SunIcon} variant="invisible" size="small" aria-label="Toggle theme" onClick={toggle} />
              </div>
            )}
          </div>

          {!narrow && (
            <div
              className={'app-sidebar-handle pane-resize-handle' + (dragging ? ' dragging' : '')}
              role="slider"
              aria-orientation="horizontal"
              aria-label="Resize sidebar"
              aria-valuemin={PANE_MIN}
              aria-valuemax={PANE_MAX}
              aria-valuenow={width}
              aria-valuetext={`Sidebar width ${width} pixels`}
              tabIndex={0}
              {...handleProps}
            />
          )}
        </div>

        <main className="app-content">
          {children}
        </main>
      </div>
    </div>
  );
}
