import { useEffect, useId } from 'react';
import { PageHeader, Stack, Tooltip } from '@primer/react';
import { ToggleRow } from '@/components/ToggleRow';
import { ReadOnlyContext } from '@/lib/access';
import { useTheme } from '@/theme/ThemeProvider';
import { DARK_THEMES, LIGHT_THEMES, ensureThemeCss, themeFile, type ThemeEntry, type ThemeName } from '@/theme/themes';

// AppearanceSection picks this browser's light and dark themes (invariant 92):
// one row of swatches per mode, the chosen one named beside them, and a
// high-contrast switch over both. Personal, so never read-only; applied on click.
export function AppearanceSection() {
  const { scheme, setScheme } = useTheme();
  // Each swatch is painted by its own theme's file: fetch the seven at the
  // contrast in force, and they fill in as each lands.
  useEffect(() => {
    for (const t of [...LIGHT_THEMES, ...DARK_THEMES]) void ensureThemeCss(themeFile(t.name, scheme.highContrast));
  }, [scheme.highContrast]);
  return (
    <ReadOnlyContext value={false}>
      <div className="form-group">
        <PageHeader>
          <PageHeader.TitleArea>
            <PageHeader.Title as="h3">Appearance</PageHeader.Title>
          </PageHeader.TitleArea>
          <PageHeader.Description>This browser&apos;s light and dark themes; the sidebar&apos;s theme menu picks which one shows.</PageHeader.Description>
        </PageHeader>
        <Stack gap="normal">
          <ThemeRow label="Light theme" themes={LIGHT_THEMES} value={scheme.light} highContrast={scheme.highContrast}
            onChange={light => setScheme({ light })} />
          <ThemeRow label="Dark theme" themes={DARK_THEMES} value={scheme.dark} highContrast={scheme.highContrast}
            onChange={dark => setScheme({ dark })} />
          <ToggleRow label="High contrast" description="Stronger contrast for the light and dark themes chosen above."
            checked={scheme.highContrast} onChange={highContrast => setScheme({ highContrast })} />
        </Stack>
      </div>
    </ReadOnlyContext>
  );
}

function ThemeRow<N extends ThemeName>({ label, themes, value, highContrast, onChange }: {
  label: string;
  themes: ThemeEntry<N>[];
  value: N;
  highContrast: boolean;
  onChange: (name: N) => void;
}) {
  const id = useId();
  const current = themes.find(t => t.name === value) ?? themes[0];
  return (
    <div className="theme-row">
      <div className="theme-row-main">
        <div id={id} className="theme-row-title">{label}</div>
        <div className="theme-row-desc"><span className="theme-row-name">{current.label}</span>{current.description}</div>
      </div>
      <div role="radiogroup" aria-labelledby={id} className="theme-swatches">
        {themes.map(t => {
          // Each swatch is painted by its own theme's tokens: Primer's CSS
          // scopes them to exactly these attributes.
          const file = themeFile(t.name, highContrast);
          return (
            <Tooltip key={t.name} text={t.label} direction="s" type="label">
              <button type="button" role="radio" aria-checked={t.name === value}
                className={'theme-swatch' + (t.wedges ? ' theme-swatch--wedges' : '')}
                data-color-mode={t.mode} data-light-theme={file} data-dark-theme={file}
                onClick={() => onChange(t.name)} />
            </Tooltip>
          );
        })}
      </div>
    </div>
  );
}
