import React from 'react';
import { api } from '/lib/api.js';

const { useState, useEffect, useCallback } = React;
const h = React.createElement;

function ChevronIcon({ expanded }) {
  return h('svg', {
    className: 'file-tree-chevron' + (expanded ? ' expanded' : ''),
    viewBox: '0 0 12 12',
    fill: 'currentColor',
  },
    h('path', { d: 'M4.7 10l-.7-.7L7.3 6 4 2.7l.7-.7L8.7 6z' }),
  );
}

function DirIcon() {
  return h('svg', { className: 'file-tree-icon', viewBox: '0 0 16 16', fill: 'currentColor' },
    h('path', { d: 'M1.75 1A1.75 1.75 0 0 0 0 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0 0 16 13.25v-8.5A1.75 1.75 0 0 0 14.25 3H7.5a.25.25 0 0 1-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1H1.75Z' }),
  );
}

function FileIcon() {
  return h('svg', { className: 'file-tree-icon', viewBox: '0 0 16 16', fill: 'currentColor' },
    h('path', { d: 'M2 1.75C2 .784 2.784 0 3.75 0h6.586c.464 0 .909.184 1.237.513l2.914 2.914c.329.328.513.773.513 1.237v9.586A1.75 1.75 0 0 1 13.25 16h-9.5A1.75 1.75 0 0 1 2 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 0 0 .25-.25V6h-2.75A1.75 1.75 0 0 1 9 4.25V1.5Zm6.75.062V4.25c0 .138.112.25.25.25h2.688l-.011-.013-2.914-2.914-.013-.011Z' }),
  );
}

function TreeNode({ entry, parentPath, selectedPath, onSelect, depth }) {
  const [expanded, setExpanded] = useState(false);
  const [children, setChildren] = useState(null);
  const fullPath = parentPath ? parentPath + '/' + entry.name : entry.name;
  const isSelected = fullPath === selectedPath;

  const toggle = useCallback(async () => {
    if (!entry.is_dir) {
      onSelect(fullPath);
      return;
    }
    if (!expanded && children === null) {
      try {
        const items = await api.files.list(fullPath);
        setChildren(items || []);
      } catch { setChildren([]); }
    }
    setExpanded(prev => !prev);
  }, [expanded, children, fullPath, entry.is_dir, onSelect]);

  const indent = depth * 12;

  return h(React.Fragment, null,
    h('div', {
      className: 'file-tree-node' + (isSelected ? ' active' : ''),
      onClick: toggle,
      style: { paddingLeft: indent + 8 + 'px' },
    },
      h('span', { className: 'file-tree-toggle' },
        entry.is_dir ? h(ChevronIcon, { expanded }) : null,
      ),
      entry.is_dir ? h(DirIcon) : h(FileIcon),
      h('span', { className: 'file-tree-name' }, entry.name),
    ),
    expanded && children && children.map(child =>
      h(TreeNode, {
        key: child.name,
        entry: child,
        parentPath: fullPath,
        selectedPath,
        onSelect,
        depth: depth + 1,
      }),
    ),
  );
}

export function FileTree({ selectedPath, onSelect, basePath = '' }) {
  const [roots, setRoots] = useState(null);

  useEffect(() => {
    api.files.list(basePath).then(items => setRoots(items || [])).catch(() => setRoots([]));
  }, [basePath]);

  if (roots === null) {
    return h('div', { className: 'file-tree-loading' }, 'Loading...');
  }

  return h('div', { className: 'file-tree' },
    roots.map(entry =>
      h(TreeNode, {
        key: entry.name,
        entry,
        parentPath: basePath,
        selectedPath,
        onSelect,
        depth: 0,
      }),
    ),
    roots.length === 0 && h('div', { className: 'file-tree-empty' }, 'No files'),
  );
}
