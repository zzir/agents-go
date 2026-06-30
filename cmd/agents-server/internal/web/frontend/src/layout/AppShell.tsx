import { type ReactNode, useCallback, useState, useEffect } from 'react';
import { PageLayout, Button, IconButton } from '@primer/react';
import { GearIcon, MoonIcon, SunIcon, ThreeBarsIcon } from '@primer/octicons-react';
import { useTheme } from '@/theme/ThemeProvider';

const NARROW_QUERY = '(max-width: 767px)';

function useNarrow(): boolean {
  const [narrow, setNarrow] = useState(() => window.matchMedia(NARROW_QUERY).matches);
  useEffect(() => {
    const mql = window.matchMedia(NARROW_QUERY);
    const handler = (e: MediaQueryListEvent) => setNarrow(e.matches);
    mql.addEventListener('change', handler);
    return () => mql.removeEventListener('change', handler);
  }, []);
  return narrow;
}

interface AppShellProps {
  onSettingsOpen: () => void;
  sidebarPane: ReactNode;
  sidebarOpen: boolean;
  onSidebarToggle: (open: boolean) => void;
  children: ReactNode;
}

export function AppShell({ onSettingsOpen, sidebarPane, sidebarOpen, onSidebarToggle, children }: AppShellProps) {
  const { theme, toggle } = useTheme();
  const narrow = useNarrow();
  const closeSidebar = useCallback(() => onSidebarToggle(false), [onSidebarToggle]);

  return (
    <div className={'app-layout' + (sidebarOpen ? ' sidebar-open' : '')}>
      {narrow && (
        <header className="mobile-header">
          <IconButton icon={ThreeBarsIcon} variant="invisible" aria-label="Open sidebar" onClick={() => onSidebarToggle(true)} />
          <span className="mobile-header-title" />
          <IconButton icon={theme === 'day' ? MoonIcon : SunIcon} variant="invisible" aria-label="Toggle theme" onClick={toggle} />
          <IconButton icon={GearIcon} variant="invisible" aria-label="Settings" onClick={onSettingsOpen} />
        </header>
      )}

      {narrow && <div className="sidebar-backdrop" onClick={closeSidebar} />}

      <PageLayout containerWidth="full" padding="none" columnGap="none" style={{ flex: 1, overflow: 'hidden' }}>
        <PageLayout.Pane
          position="start"
          resizable
          width={{ min: '300px', default: '300px', max: '300px' }}
          divider="line"
          padding="none"
          className="app-sidebar-pane"
        >
          <div className="sidebar-container">
            <div className="sidebar-body">
              {sidebarPane}
            </div>
            {!narrow && (
              <div className="sidebar-footer">
                <Button variant="invisible" size="small" onClick={onSettingsOpen}>
                  <GearIcon size={16} /> Settings
                </Button>
                <Button variant="invisible" size="small" onClick={toggle} aria-label="Toggle theme">
                  {theme === 'day' ? <MoonIcon size={16} /> : <SunIcon size={16} />}
                </Button>
              </div>
            )}
          </div>
        </PageLayout.Pane>

        <PageLayout.Content padding="none">
          <main className="app-content">
            {children}
          </main>
        </PageLayout.Content>
      </PageLayout>
    </div>
  );
}
