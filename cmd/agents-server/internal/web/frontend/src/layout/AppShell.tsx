import { type ReactNode, useCallback, useRef } from 'react';
import { ActionList, ActionMenu, IconButton, useOnEscapePress } from '@primer/react';
import { DeviceDesktopIcon, MoonIcon, SidebarExpandIcon, SunIcon, ThreeBarsIcon } from '@primer/octicons-react';
import { useTheme, type ThemePreference } from '@/theme/ThemeProvider';
import { useNarrow, useResizablePane } from '@/lib/hooks';
import { UserMenu } from '@/layout/UserMenu';

const PANE_WIDTH_KEY = 'paneWidth';
const PANE_MIN = 260;
const PANE_MAX = 400;
const PANE_DEFAULT = 300;
// As wide as the top bars are tall (--topbar-height): the rail's first button
// sits on their line.
const RAIL_WIDTH = 48;

interface AppShellProps {
  onSettingsOpen: () => void;
  sidebarPane: ReactNode;
  // The sidebar actions the rail keeps: Workflows and New (invariant 68).
  railActions: ReactNode;
  sidebarOpen: boolean;
  onSidebarToggle: (open: boolean) => void;
  children: ReactNode;
}

export function AppShell({ onSettingsOpen, sidebarPane, railActions, sidebarOpen, onSidebarToggle, children }: AppShellProps) {
  const { theme, preference, setPreference } = useTheme();
  // Light, dark, or the system's: one menu behind the current theme's icon.
  const themeMenu = (size?: 'small') => (
    <ActionMenu>
      <ActionMenu.Anchor>
        <IconButton icon={preference === 'system' ? DeviceDesktopIcon : theme === 'day' ? SunIcon : MoonIcon} variant="invisible" size={size} aria-label="Theme" />
      </ActionMenu.Anchor>
      <ActionMenu.Overlay>
        <ActionList selectionVariant="single">
          {(['light', 'dark', 'system'] as ThemePreference[]).map(p => (
            <ActionList.Item key={p} selected={preference === p} onSelect={() => setPreference(p)}>
              {p === 'light' ? 'Light' : p === 'dark' ? 'Dark' : 'System'}
            </ActionList.Item>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
  const narrow = useNarrow();
  const closeSidebar = useCallback(() => onSidebarToggle(false), [onSidebarToggle]);
  // The drawer is a CSS shift with no dialog behind it, so Escape is wired here
  // (through Primer's registry, so an overlay open on top takes the key first).
  const menuButtonRef = useRef<HTMLButtonElement>(null);
  const drawerOpen = narrow && sidebarOpen;
  useOnEscapePress(useCallback((e: KeyboardEvent) => {
    if (!drawerOpen) return;
    e.preventDefault();
    onSidebarToggle(false);
    menuButtonRef.current?.focus();
  }, [drawerOpen, onSidebarToggle]));

  const { width, collapsed, snapping, dragging, expand, handleProps } = useResizablePane({ storageKey: PANE_WIDTH_KEY, min: PANE_MIN, max: PANE_MAX, defaultWidth: PANE_DEFAULT, edge: 'left', collapsedWidth: RAIL_WIDTH });
  // The rail is a desktop shape; the narrow layout's drawer ignores it.
  const rail = !narrow && collapsed;
  const paneWidth = rail ? RAIL_WIDTH : width;

  return (
    <div className={'app-layout' + (sidebarOpen ? ' sidebar-open' : '')}>
      {narrow && (
        <header className="mobile-header">
          <IconButton ref={menuButtonRef} icon={ThreeBarsIcon} variant="invisible" aria-label="Open sidebar" onClick={() => onSidebarToggle(true)} />
          <div className="mobile-header-actions">
            {themeMenu()}
            <UserMenu onSettingsOpen={onSettingsOpen} compact align="end" />
          </div>
        </header>
      )}

      {narrow && <div className="sidebar-backdrop" role="presentation" onClick={closeSidebar} />}

      <div className="app-body">
        <div className={'app-sidebar-pane' + (rail ? ' rail' : '') + (snapping && !narrow ? ' snapping' : '')} style={narrow ? undefined : { width: paneWidth }}>
          <div className="sidebar-clip">
            {/* Stays mounted under the rail, so expanding shows the list and
                its search as they were. */}
            <div className="sidebar-container" style={narrow ? undefined : { minWidth: PANE_MIN }}>
              <div className="sidebar-body">
                {sidebarPane}
              </div>
              {!narrow && (
                <div className="sidebar-footer">
                  <UserMenu onSettingsOpen={onSettingsOpen} />
                  {themeMenu('small')}
                </div>
              )}
            </div>
            {rail && (
              <div className="sidebar-rail">
                <IconButton icon={SidebarExpandIcon} variant="invisible" aria-label="Expand sidebar" onClick={expand} />
                {railActions}
                <div className="sidebar-rail-foot">
                  <UserMenu onSettingsOpen={onSettingsOpen} compact />
                </div>
              </div>
            )}
          </div>

          {!narrow && (
            <div
              className={'app-sidebar-handle pane-resize-handle' + (dragging ? ' dragging' : '')}
              role="slider"
              aria-orientation="horizontal"
              aria-label="Resize sidebar"
              aria-valuemin={RAIL_WIDTH}
              aria-valuemax={PANE_MAX}
              aria-valuenow={paneWidth}
              aria-valuetext={rail ? 'Sidebar collapsed to a rail' : `Sidebar width ${width} pixels`}
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
