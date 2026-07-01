import { type ReactNode, useCallback, useState, useEffect, useRef } from 'react';
import { Button, IconButton } from '@primer/react';
import { GearIcon, MoonIcon, SunIcon, ThreeBarsIcon } from '@primer/octicons-react';
import { useTheme } from '@/theme/ThemeProvider';

const NARROW_QUERY = '(max-width: 767px)';
const PANE_WIDTH_KEY = 'paneWidth';
const PANE_MIN = 260;
const PANE_MAX = 400;
const PANE_DEFAULT = 300;
const ARROW_KEY_STEP = 10;

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

function clampWidth(n: number): number {
  return Math.min(PANE_MAX, Math.max(PANE_MIN, n));
}

function readStoredWidth(): number {
  try {
    const raw = localStorage.getItem(PANE_WIDTH_KEY);
    if (raw === null) return PANE_DEFAULT;
    const n = Math.round(Number(raw));
    if (!Number.isFinite(n) || n <= 0) return PANE_DEFAULT;
    return clampWidth(n);
  } catch {
    return PANE_DEFAULT;
  }
}

function saveWidth(width: number): void {
  try {
    localStorage.setItem(PANE_WIDTH_KEY, String(Math.round(width)));
  } catch {
    // Ignore write errors (private browsing, quota exceeded, etc.)
  }
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

  const [width, setWidth] = useState(readStoredWidth);
  const widthRef = useRef(width);
  widthRef.current = width;
  const dragStartXRef = useRef(0);
  const dragStartWidthRef = useRef(0);

  const handlePointerDown = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // Pointer capture is a nice-to-have; ignore if unsupported/unavailable.
    }
    dragStartXRef.current = e.clientX;
    dragStartWidthRef.current = widthRef.current;
  }, []);

  const handlePointerMove = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    e.preventDefault();
    const delta = e.clientX - dragStartXRef.current;
    const next = clampWidth(dragStartWidthRef.current + delta);
    if (next !== widthRef.current) setWidth(next);
  }, []);

  const handlePointerUp = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    saveWidth(widthRef.current);
  }, []);

  const handleKeyDown = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    const delta = e.key === 'ArrowLeft' ? -ARROW_KEY_STEP : ARROW_KEY_STEP;
    const next = clampWidth(widthRef.current + delta);
    if (next !== widthRef.current) {
      setWidth(next);
      saveWidth(next);
    }
  }, []);

  const handleDoubleClick = useCallback(() => {
    setWidth(PANE_DEFAULT);
    saveWidth(PANE_DEFAULT);
  }, []);

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

      <div className="app-body">
        <div className="app-sidebar-pane" style={narrow ? undefined : { width }}>
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

          {!narrow && (
            <div
              className="app-sidebar-handle"
              role="slider"
              aria-orientation="horizontal"
              aria-label="Resize sidebar"
              aria-valuemin={PANE_MIN}
              aria-valuemax={PANE_MAX}
              aria-valuenow={width}
              aria-valuetext={`Sidebar width ${width} pixels`}
              tabIndex={0}
              onPointerDown={handlePointerDown}
              onPointerMove={handlePointerMove}
              onPointerUp={handlePointerUp}
              onLostPointerCapture={handlePointerUp}
              onKeyDown={handleKeyDown}
              onDoubleClick={handleDoubleClick}
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
