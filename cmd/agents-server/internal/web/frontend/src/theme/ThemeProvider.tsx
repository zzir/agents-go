import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from 'react';
import { ThemeProvider as PrimerThemeProvider, BaseStyles } from '@primer/react';
import { DEFAULT_SCHEME, ensureThemeCss, primerScheme, readScheme, themeFile, writeScheme, type Scheme } from '@/theme/themes';

// The person's choice: a theme, or the system's. Stored under localStorage
// 'theme' as 'light' | 'dark' | 'system' (an older 'light'/'dark' reads as is).
export type ThemePreference = 'light' | 'dark' | 'system';

interface ThemeContextValue {
  // The resolved theme Primer is given.
  theme: 'day' | 'night';
  preference: ThemePreference;
  setPreference: (p: ThemePreference) => void;
  // Which light and which dark theme each mode shows, and at what contrast.
  scheme: Scheme;
  setScheme: (patch: Partial<Scheme>) => void;
}

const ThemeContext = createContext<ThemeContextValue>({
  theme: 'day', preference: 'system', setPreference: () => {}, scheme: DEFAULT_SCHEME, setScheme: () => {},
});

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
  const [scheme, setSchemeState] = useState<Scheme>(readScheme);
  const [dark, setDark] = useState(systemDark);
  const setPreference = useCallback((p: ThemePreference) => {
    setPreferenceState(p);
    try { localStorage.setItem('theme', p); } catch { /* a private window: the choice lasts the page */ }
  }, []);
  const setScheme = useCallback((patch: Partial<Scheme>) => {
    setSchemeState(prev => {
      const next = { ...prev, ...patch };
      writeScheme(next);
      return next;
    });
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
  const lightFile = themeFile(scheme.light, scheme.highContrast);
  const darkFile = themeFile(scheme.dark, scheme.highContrast);
  useEffect(() => {
    // <html> alone carries the theme (invariant 92): Primer's CSS keys on these
    // three attributes, and the terminal and mermaid watch them. A file is
    // stamped once its CSS is on the page; one that failed to load is not.
    let cancelled = false;
    Promise.all([ensureThemeCss(lightFile), ensureThemeCss(darkFile)]).then(([lightOk, darkOk]) => {
      if (cancelled) return;
      const h = document.documentElement;
      const isDark = theme === 'night';
      h.setAttribute('data-color-mode', isDark ? 'dark' : 'light');
      h.setAttribute('data-light-theme', lightOk ? lightFile : 'light');
      h.setAttribute('data-dark-theme', darkOk ? darkFile : 'dark');

      const color = getComputedStyle(h).getPropertyValue('--bgColor-default').trim() || (isDark ? '#0d1117' : '#ffffff');
      const old = document.querySelector('meta[name="theme-color"]');
      if (old) old.remove();
      const meta = document.createElement('meta');
      meta.name = 'theme-color';
      meta.content = color;
      document.head.appendChild(meta);
    });
    return () => { cancelled = true; };
  }, [theme, lightFile, darkFile]);

  // contextOnly: no wrapper div with its own theme attributes, which would
  // override <html>'s for the five combined names Primer's object lacks.
  return (
    <PrimerThemeProvider
      colorMode={theme}
      dayScheme={primerScheme(scheme.light, scheme.highContrast)}
      nightScheme={primerScheme(scheme.dark, scheme.highContrast)}
      contextOnly
    >
      <BaseStyles>
        <ThemeContext.Provider value={{ theme, preference, setPreference, scheme, setScheme }}>
          {children}
        </ThemeContext.Provider>
      </BaseStyles>
    </PrimerThemeProvider>
  );
}
