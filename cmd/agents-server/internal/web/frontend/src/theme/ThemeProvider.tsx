import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from 'react';
import { ThemeProvider as PrimerThemeProvider, BaseStyles } from '@primer/react';

interface ThemeContextValue {
  theme: 'day' | 'night';
  toggle: () => void;
}

const ThemeContext = createContext<ThemeContextValue>({ theme: 'day', toggle: () => {} });

export function useTheme(): ThemeContextValue {
  return useContext(ThemeContext);
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<'day' | 'night'>(() => {
    return (localStorage.getItem('theme') === 'dark' ? 'night' : 'day');
  });

  const toggle = useCallback(() => {
    setTheme(t => {
      const next = t === 'day' ? 'night' : 'day';
      localStorage.setItem('theme', next === 'night' ? 'dark' : 'light');
      return next;
    });
  }, []);

  useEffect(() => {
    const dark = theme === 'night';
    // Keep <html data-color-mode> in sync so consumers keyed off it stay live:
    // syntax.css ([data-color-mode="dark"] .hljs-*) flips code colors for free
    // (pure CSS, no re-render), and MermaidBlock's MutationObserver fires so
    // diagrams re-render with the new theme. theme-init.js only sets it once.
    document.documentElement.setAttribute('data-color-mode', dark ? 'dark' : 'light');

    const color = dark ? '#0d1117' : '#ffffff';
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
        <ThemeContext.Provider value={{ theme, toggle }}>
          {children}
        </ThemeContext.Provider>
      </BaseStyles>
    </PrimerThemeProvider>
  );
}
