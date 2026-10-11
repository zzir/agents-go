import { Fragment, useEffect, useId } from 'react';
import { PageHeader, SegmentedControl, Stack, Tooltip } from '@primer/react';
import { ToggleRow } from '@/components/ToggleRow';
import { ReadOnlyContext } from '@/lib/access';
import { useTheme, type ThemePreference } from '@/theme/ThemeProvider';
import { THEMES, ensureThemeCss, themeFile, type ThemeEntry } from '@/theme/themes';

const MODES: [ThemePreference, string][] = [['light', 'Light'], ['dark', 'Dark'], ['system', 'System']];

// AppearanceSection is this browser's theme (invariant 92): the mode the
// sidebar menu also sets, the seven themes in one row, and a high-contrast
// switch over both picks. Personal, so never read-only; applied on click.
export function AppearanceSection() {
  const { theme, preference, setPreference, scheme, setScheme } = useTheme();
  // Each swatch is painted by its own theme's file: fetch the seven at the
  // contrast in force, and they fill in as each lands.
  useEffect(() => {
    for (const t of THEMES) void ensureThemeCss(themeFile(t.name, scheme.highContrast));
  }, [scheme.highContrast]);

  // The theme on screen; under system, also the pick the other time of day shows.
  const shown = theme === 'night' ? scheme.dark : scheme.light;
  const other = preference === 'system' ? (theme === 'night' ? scheme.light : scheme.dark) : null;
  const current = THEMES.find(t => t.name === shown) ?? THEMES[0];
  const pick = (t: ThemeEntry) => {
    if (t.mode === 'light') setScheme({ light: t.name });
    else setScheme({ dark: t.name });
    // A fixed mode follows the click: what is picked is what shows.
    if (preference !== 'system' && preference !== t.mode) setPreference(t.mode);
  };
  const id = useId();
  return (
    <ReadOnlyContext value={false}>
      <div className="form-group">
        <PageHeader>
          <PageHeader.TitleArea>
            <PageHeader.Title as="h3">Appearance</PageHeader.Title>
          </PageHeader.TitleArea>
          <PageHeader.Description>This browser&apos;s theme; the sidebar&apos;s theme menu is the same choice.</PageHeader.Description>
        </PageHeader>
        <Stack gap="normal">
          <div className="theme-row">
            <div className="theme-row-main">
              <div className="theme-row-title">Theme mode</div>
              <div className="theme-row-desc">Light, dark, or whichever the system is using.</div>
            </div>
            {/* onChange plus `selected` is Primer's controlled form: a swatch
                click also moves the mode, which an uncontrolled control would not show. */}
            <SegmentedControl aria-label="Theme mode" size="small" onChange={i => setPreference(MODES[i][0])}>
              {MODES.map(([value, text]) => (
                <SegmentedControl.Button key={value} selected={preference === value}>{text}</SegmentedControl.Button>
              ))}
            </SegmentedControl>
          </div>
          <div className="theme-row theme-row--themes">
            <div id={id} className="theme-row-title">Theme</div>
            <div role="radiogroup" aria-labelledby={id} className="theme-swatches">
              {THEMES.map((t, i) => {
                // Each swatch is painted by its own theme's tokens: Primer's CSS
                // scopes them to exactly these attributes.
                const file = themeFile(t.name, scheme.highContrast);
                const ring = t.name === shown ? 'shown' : t.name === other ? 'other' : undefined;
                return (
                  <Fragment key={t.name}>
                    {i > 0 && t.mode !== THEMES[i - 1].mode && <span className="theme-swatches-divider" aria-hidden />}
                    <Tooltip text={t.label} direction="s" type="label">
                      <button type="button" role="radio" aria-checked={ring === 'shown'} data-ring={ring}
                        className={'theme-swatch' + (t.wedges ? ' theme-swatch--wedges' : '')}
                        data-color-mode={t.mode} data-light-theme={file} data-dark-theme={file}
                        onClick={() => pick(t)} />
                    </Tooltip>
                  </Fragment>
                );
              })}
            </div>
            <div className="theme-row-desc theme-row-desc--wide"><span className="theme-row-name">{current.label}</span>{current.description}</div>
          </div>
          <ToggleRow label="High contrast" description="Stronger contrast for the light and dark themes."
            checked={scheme.highContrast} onChange={highContrast => setScheme({ highContrast })} />
        </Stack>
      </div>
    </ReadOnlyContext>
  );
}
