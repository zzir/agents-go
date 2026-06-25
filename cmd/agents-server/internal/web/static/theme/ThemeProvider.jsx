import React from 'react';

const { createContext, useContext, useState, useEffect } = React;

const ThemeContext = createContext({ theme: 'light', toggle: () => {} });

export function useTheme() {
  return useContext(ThemeContext);
}

export function ThemeProvider({ children }) {
  const [theme, setTheme] = useState(() => {
    return localStorage.getItem('theme') || 'light';
  });

  useEffect(() => {
    const el = document.documentElement;
    el.setAttribute('data-color-mode', theme);
    el.setAttribute('data-light-theme', 'light');
    el.setAttribute('data-dark-theme', 'dark');
    localStorage.setItem('theme', theme);
  }, [theme]);

  const toggle = () => setTheme(t => t === 'light' ? 'dark' : 'light');

  return React.createElement(
    ThemeContext.Provider,
    { value: { theme, toggle } },
    children
  );
}
