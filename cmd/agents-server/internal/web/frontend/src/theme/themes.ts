// The Primer themes a browser can pick from: three light, four dark, each with
// a high-contrast twin — the fourteen files main.tsx loads.
export type LightTheme = 'light' | 'light_colorblind' | 'light_tritanopia';
export type DarkTheme = 'dark' | 'dark_dimmed' | 'dark_colorblind' | 'dark_tritanopia';
export type ThemeName = LightTheme | DarkTheme;

interface ThemeText {
  label: string;
  description: string;
  // Whether the swatch shows the theme's success and danger hues — the two
  // colors a colorblind theme exists to change.
  wedges: boolean;
}

// A theme belongs to one mode: a light one is only ever the light pick, a dark
// one the dark pick.
export type ThemeEntry =
  | (ThemeText & { mode: 'light'; name: LightTheme })
  | (ThemeText & { mode: 'dark'; name: DarkTheme });

// The seven, in the order the row shows them: light first, then dark.
export const THEMES: ThemeEntry[] = [
  { name: 'light', mode: 'light', label: 'Light default', description: 'The standard light theme, full color contrast and brightness.', wedges: false },
  { name: 'light_colorblind', mode: 'light', label: 'Light protanopia and deuteranopia', description: 'Reds and greens are easier to tell apart.', wedges: true },
  { name: 'light_tritanopia', mode: 'light', label: 'Light tritanopia', description: 'Blues and greens, and yellows and purples, are easier to tell apart.', wedges: true },
  { name: 'dark', mode: 'dark', label: 'Dark default', description: 'The standard dark theme, full color contrast and brightness.', wedges: false },
  { name: 'dark_colorblind', mode: 'dark', label: 'Dark protanopia and deuteranopia', description: 'Reds and greens are easier to tell apart.', wedges: true },
  { name: 'dark_tritanopia', mode: 'dark', label: 'Dark tritanopia', description: 'Blues and greens, and yellows and purples, are easier to tell apart.', wedges: true },
  { name: 'dark_dimmed', mode: 'dark', label: 'Soft dark', description: 'Reduced contrast on a dark background, for low-light rooms.', wedges: false },
];

// Scheme is the browser's choice beyond light-or-dark: which light theme,
// which dark theme, and whether both run at high contrast (invariant 92).
export interface Scheme {
  light: LightTheme;
  dark: DarkTheme;
  highContrast: boolean;
}

export const DEFAULT_SCHEME: Scheme = { light: 'light', dark: 'dark', highContrast: false };

// The attributes <html> carries for Primer's CSS; what watches the theme observes these.
export const THEME_ATTRS = ['data-color-mode', 'data-light-theme', 'data-dark-theme'];

const KEYS = { light: 'theme.light', dark: 'theme.dark', highContrast: 'theme.highContrast' } as const;

// readScheme reads the stored choice; a missing, unknown or throwing value is the default.
export function readScheme(): Scheme {
  const get = (key: string): string | null => { try { return localStorage.getItem(key); } catch { return null; } };
  const light = get(KEYS.light);
  const dark = get(KEYS.dark);
  const known = (mode: 'light' | 'dark', v: string | null) => THEMES.some(t => t.mode === mode && t.name === v);
  return {
    light: known('light', light) ? light as LightTheme : DEFAULT_SCHEME.light,
    dark: known('dark', dark) ? dark as DarkTheme : DEFAULT_SCHEME.dark,
    highContrast: get(KEYS.highContrast) === 'true',
  };
}

export function writeScheme(s: Scheme): void {
  try {
    localStorage.setItem(KEYS.light, s.light);
    localStorage.setItem(KEYS.dark, s.dark);
    localStorage.setItem(KEYS.highContrast, String(s.highContrast));
  } catch { /* a private window: the choice lasts the page */ }
}

// themeFile is the name Primer's CSS keys a theme on: the base name, or its
// high-contrast twin.
export function themeFile(name: ThemeName, highContrast: boolean): string {
  return highContrast ? `${name}_high_contrast` : name;
}

// primerScheme is the name Primer's theme OBJECT is given. It defines nine
// schemes where the CSS has fourteen, and logs an error for any other name.
const PRIMER_SCHEMES = new Set([
  'light', 'light_high_contrast', 'light_colorblind', 'light_tritanopia',
  'dark', 'dark_dimmed', 'dark_high_contrast', 'dark_colorblind', 'dark_tritanopia',
]);
export function primerScheme(name: ThemeName, highContrast: boolean): string {
  const file = themeFile(name, highContrast);
  return PRIMER_SCHEMES.has(file) ? file : name;
}

// The twelve theme files beyond the two main.tsx bundles, fetched the first
// time a scheme needs one (18 KB gzipped each) — invariant 92.
const LAZY_CSS: Record<string, () => Promise<unknown>> = {
  light_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/light-high-contrast.css'),
  light_colorblind: () => import('@primer/primitives/dist/css/functional/themes/light-colorblind.css'),
  light_colorblind_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/light-colorblind-high-contrast.css'),
  light_tritanopia: () => import('@primer/primitives/dist/css/functional/themes/light-tritanopia.css'),
  light_tritanopia_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/light-tritanopia-high-contrast.css'),
  dark_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/dark-high-contrast.css'),
  dark_dimmed: () => import('@primer/primitives/dist/css/functional/themes/dark-dimmed.css'),
  dark_dimmed_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/dark-dimmed-high-contrast.css'),
  dark_colorblind: () => import('@primer/primitives/dist/css/functional/themes/dark-colorblind.css'),
  dark_colorblind_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/dark-colorblind-high-contrast.css'),
  dark_tritanopia: () => import('@primer/primitives/dist/css/functional/themes/dark-tritanopia.css'),
  dark_tritanopia_high_contrast: () => import('@primer/primitives/dist/css/functional/themes/dark-tritanopia-high-contrast.css'),
};
const loading = new Map<string, Promise<boolean>>();

// ensureThemeCss resolves true once the file's CSS is on the page, false when
// its fetch failed (a stale chunk); a bundled file is true at once.
export function ensureThemeCss(file: string): Promise<boolean> {
  const load = LAZY_CSS[file];
  if (!load) return Promise.resolve(true);
  let p = loading.get(file);
  if (!p) {
    p = load().then(() => true, () => { loading.delete(file); return false; });
    loading.set(file, p);
  }
  return p;
}

// ensureSchemeCss fetches what a scheme shows: its light and its dark file.
export function ensureSchemeCss(s: Scheme): Promise<[boolean, boolean]> {
  return Promise.all([ensureThemeCss(themeFile(s.light, s.highContrast)), ensureThemeCss(themeFile(s.dark, s.highContrast))]);
}
