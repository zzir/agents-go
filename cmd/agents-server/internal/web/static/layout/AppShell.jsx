import React from 'react';
import { useTheme } from '/theme/ThemeProvider.jsx';

const { useState, useCallback } = React;
const h = React.createElement;

const NAV_ITEMS = [
  { key: 'chat',  label: 'Chat',  icon: iconChat },
  { key: 'files', label: 'Files', icon: iconFolder },
];

export function AppShell({ view, onViewChange, onSettingsOpen, sidebarPane, children }) {
  const { theme, toggle } = useTheme();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);

  const handleNav = useCallback((key) => {
    onViewChange(key);
    setMobileNavOpen(false);
  }, [onViewChange]);

  return h('div', { className: 'app-layout' },
    h('aside', { className: 'app-sidebar' + (mobileNavOpen ? ' mobile-open' : '') },
      h('nav', { className: 'sidebar-tabs SegmentedControl', role: 'radiogroup', 'aria-label': 'Navigation' },
        NAV_ITEMS.map(item =>
          h('button', {
            key: item.key,
            className: 'sidebar-tab SegmentedControl-item',
            role: 'radio',
            'aria-checked': view === item.key ? 'true' : 'false',
            onClick: () => handleNav(item.key),
          },
            h('span', { className: 'sidebar-tab-icon' }, item.icon()),
            item.label,
          ),
        ),
      ),
      h('div', { className: 'sidebar-body' }, sidebarPane),
      h('div', { className: 'sidebar-footer' },
        h('button', {
          className: 'sidebar-footer-btn',
          onClick: () => { onSettingsOpen(); setMobileNavOpen(false); },
        },
          h('span', { className: 'sidebar-footer-icon' }, iconGear()),
          'Settings',
        ),
        h('button', { className: 'sidebar-footer-btn', onClick: toggle },
          theme === 'light' ? iconMoon() : iconSun(),
        ),
      ),
    ),
    h('main', { className: 'app-content' }, children),
    h('nav', { className: 'bottom-bar' },
      h('div', { className: 'bottom-bar-inner' },
        NAV_ITEMS.map(item =>
          h('button', {
            key: item.key,
            className: 'bottom-bar-item' + (view === item.key ? ' active' : ''),
            onClick: () => handleNav(item.key),
          },
            item.icon(),
            h('span', null, item.label),
          ),
        ),
        h('button', {
          className: 'bottom-bar-item',
          onClick: () => { onSettingsOpen(); setMobileNavOpen(false); },
        },
          iconGear(),
          h('span', null, 'Settings'),
        ),
      ),
    ),
  );
}

function iconChat() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M1 2.75C1 1.784 1.784 1 2.75 1h10.5c.966 0 1.75.784 1.75 1.75v7.5A1.75 1.75 0 0 1 13.25 12H9.06l-2.573 2.573A1.458 1.458 0 0 1 4 13.543V12H2.75A1.75 1.75 0 0 1 1 10.25Zm1.5 0v7.5c0 .138.112.25.25.25h2a.75.75 0 0 1 .75.75v2.19l2.72-2.72a.749.749 0 0 1 .53-.22h4.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25H2.75a.25.25 0 0 0-.25.25Z' }),
  );
}
function iconFolder() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M1.75 1A1.75 1.75 0 0 0 0 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0 0 16 13.25v-8.5A1.75 1.75 0 0 0 14.25 3H7.5a.25.25 0 0 1-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1ZM1.5 2.75a.25.25 0 0 1 .25-.25H5c.09 0 .176.04.232.107l.896 1.195A1.75 1.75 0 0 0 7.5 4.5h6.75a.25.25 0 0 1 .25.25v8.5a.25.25 0 0 1-.25.25H1.75a.25.25 0 0 1-.25-.25Z' }),
  );
}
function iconGear() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M8 0a8.2 8.2 0 0 1 .701.031C9.444.076 9.99.41 10.41.95l.04.05a2.06 2.06 0 0 0 1.55.71 2.06 2.06 0 0 0 .51-.07l.06-.02c.63-.18 1.3-.02 1.78.42.49.44.71 1.09.58 1.72l-.02.06a2.06 2.06 0 0 0 .21 1.57 2.06 2.06 0 0 0 1.24.94l.07.02c.63.18 1.1.67 1.24 1.3a2.06 2.06 0 0 1-.46 1.68l-.04.05a2.06 2.06 0 0 0-.48 1.51 2.06 2.06 0 0 0 .48 1.04l.04.05c.39.51.46 1.2.17 1.78-.29.58-.84.94-1.45.96h-.07a2.06 2.06 0 0 0-1.51.48 2.06 2.06 0 0 0-.71 1.24l-.02.07c-.12.63-.56 1.14-1.18 1.33-.62.19-1.29.04-1.78-.39l-.05-.04a2.06 2.06 0 0 0-1.51-.48 2.06 2.06 0 0 0-1.04.48l-.05.04c-.5.43-1.16.58-1.78.39-.62-.19-1.06-.7-1.18-1.33l-.02-.07a2.06 2.06 0 0 0-.71-1.24 2.06 2.06 0 0 0-1.51-.48h-.07c-.61-.02-1.16-.38-1.45-.96-.29-.58-.22-1.27.17-1.78l.04-.05a2.06 2.06 0 0 0 .48-1.04 2.06 2.06 0 0 0-.48-1.51l-.04-.05a2.06 2.06 0 0 1-.46-1.68c.14-.63.61-1.12 1.24-1.3l.07-.02a2.06 2.06 0 0 0 1.24-.94 2.06 2.06 0 0 0 .21-1.57l-.02-.06c-.13-.63.09-1.28.58-1.72.48-.44 1.15-.6 1.78-.42l.06.02a2.06 2.06 0 0 0 .51.07 2.06 2.06 0 0 0 1.55-.71l.04-.05C6.01.41 6.556.076 7.299.031A8.2 8.2 0 0 1 8 0ZM8 5a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z' }),
  );
}
function iconMoon() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M9.598 1.591a.749.749 0 0 1 .785-.175 7.001 7.001 0 1 1-8.967 8.967.75.75 0 0 1 .961-.96 5.5 5.5 0 0 0 7.22-7.832Z' }),
  );
}
function iconSun() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M8 12a4 4 0 1 1 0-8 4 4 0 0 1 0 8Zm0-1.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5Zm5.657-8.157a.75.75 0 0 1 0 1.061l-1.061 1.06a.749.749 0 1 1-1.06-1.06l1.06-1.06a.75.75 0 0 1 1.06 0Zm-9.193 9.193a.75.75 0 0 1 0 1.06l-1.06 1.061a.75.75 0 1 1-1.061-1.06l1.06-1.061a.75.75 0 0 1 1.06 0ZM8 0a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0V.75A.75.75 0 0 1 8 0ZM3 8a.75.75 0 0 1-.75.75H.75a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 3 8Zm13 0a.75.75 0 0 1-.75.75h-1.5a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 16 8Zm-8 5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 13Z' }),
  );
}
