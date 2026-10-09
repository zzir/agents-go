import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from 'react';
import { ThemeProvider as PrimerThemeProvider, BaseStyles } from '@primer/react';

// The person's choice: a theme, or the system's. Stored under localStorage
// 'theme' as 'light' | 'dark' | 'system' (an older 'light'/'dark' reads as is).
export type ThemePreference = 'light' | 'dark' | 'system';

interface ThemeContextValue {
  // The resolved theme Primer is given.
  theme: 'day' | 'night';
  preference: ThemePreference;
  setPreference: (p: ThemePreference) => void;
}

const ThemeContext = createContext<ThemeContextValue>({ theme: 'day', preference: 'system', setPreference: () => {} });

export function useTheme(): ThemeContextValue {
  return useContext(ThemeContext);
}

const DARK_QUERY = '(prefers-color-scheme: dark)';

// readPreference reads the stored choice; storage that throws or holds
// anything else is the default, system.
export function readPreference(): ThemePreference {
  try {
    const v = localStorage.getItem('theme');
    return v === 'light' || v === 'dark' || v === 'system' ? v : 'system';
  } catch {
    return 'system';
  }
}

function systemDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(DARK_QUERY).matches;
}

// resolveTheme is the theme a preference means right now.
export function resolveTheme(p: ThemePreference, dark: boolean): 'day' | 'night' {
  if (p === 'system') return dark ? 'night' : 'day';
  return p === 'dark' ? 'night' : 'day';
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [preference, setPreferenceState] = useState<ThemePreference>(readPreference);
  const [dark, setDark] = useState(systemDark);
  const setPreference = useCallback((p: ThemePreference) => {
    setPreferenceState(p);
    try { localStorage.setItem('theme', p); } catch { /* a private window: the choice lasts the page */ }
  }, []);

  // The system's theme, followed live while the preference is system.
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia(DARK_QUERY);
    const onChange = (e: MediaQueryListEvent) => setDark(e.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  const theme = resolveTheme(preference, dark);
  useEffect(() => {
    const isDark = theme === 'night';
    // Keep <html data-color-mode> in sync: syntax.css keys code colors off it,
    // and MermaidBlock's MutationObserver re-renders diagrams on the flip.
    document.documentElement.setAttribute('data-color-mode', isDark ? 'dark' : 'light');

    const color = isDark ? '#0d1117' : '#ffffff';
    const old = document.querySelector('meta[name="theme-color"]');
    if (old) old.remove();
    const meta = document.createElement('meta');
    meta.name = 'theme-color';
    meta.content = color;
    document.head.appendChild(meta);
  }, [theme]);

  return (
    <PrimerThemeProvider colorMode={theme} preventSSRMismatch>
      <BaseStyles>
        <ThemeContext.Provider value={{ theme, preference, setPreference }}>
          {children}
        </ThemeContext.Provider>
      </BaseStyles>
    </PrimerThemeProvider>
  );
}
