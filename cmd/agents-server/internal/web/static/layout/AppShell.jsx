import React from 'react';
import { useTheme } from '/theme/ThemeProvider.jsx';
import { iconMenu, iconChat, iconFolder, iconGear, iconMoon, iconSun } from '/lib/icons.js';

const { useState, useCallback } = React;
const h = React.createElement;

const NAV_ITEMS = [
  { key: 'chat',  label: 'Chat',  icon: iconChat },
  { key: 'files', label: 'Files', icon: iconFolder },
];

export function AppShell({ view, onViewChange, onSettingsOpen, sidebarPane, onSidebarClose, children }) {
  const { theme, toggle } = useTheme();
  const [sidebarOpen, setSidebarOpen] = useState(false);

  const handleNav = useCallback((key) => {
    onViewChange(key);
    setSidebarOpen(false);
  }, [onViewChange]);

  const closeSidebar = useCallback(() => {
    setSidebarOpen(false);
    if (onSidebarClose) onSidebarClose();
  }, [onSidebarClose]);

  return h('div', { className: 'app-layout' },
    h('aside', { className: 'app-sidebar' + (sidebarOpen ? ' mobile-open' : '') },
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
      h('div', { className: 'sidebar-body', onClick: (e) => {
        if (e.target.closest('.NavList-item, .file-tree-node')) setSidebarOpen(false);
      } }, sidebarPane),
      h('div', { className: 'sidebar-footer' },
        h('button', {
          className: 'sidebar-footer-btn btn-invisible',
          onClick: () => { onSettingsOpen(); setSidebarOpen(false); },
        },
          h('span', { className: 'sidebar-footer-icon' }, iconGear()),
          'Settings',
        ),
        h('button', { className: 'sidebar-footer-btn btn-invisible', onClick: toggle },
          theme === 'light' ? iconMoon() : iconSun(),
        ),
      ),
    ),
    sidebarOpen && h('div', { className: 'sidebar-backdrop', onClick: closeSidebar }),
    h('div', { className: 'mobile-header' },
      h('button', { className: 'mobile-header-btn btn-invisible', onClick: () => setSidebarOpen(true), 'aria-label': 'Menu' },
        iconMenu(),
      ),
      h('span', { className: 'mobile-header-title' }, view === 'chat' ? 'Chat' : 'Files'),
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
          onClick: () => { onSettingsOpen(); setSidebarOpen(false); },
        },
          iconGear(),
          h('span', null, 'Settings'),
        ),
      ),
    ),
  );
}
