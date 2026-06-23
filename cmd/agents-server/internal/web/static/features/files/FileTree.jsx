import React from 'react';
import { api } from '/lib/api.js';

const { useState, useEffect, useCallback } = React;
const h = React.createElement;

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

  const indent = depth * 16;

  return h(React.Fragment, null,
    h('div', {
      className: 'file-tree-node' + (isSelected ? ' active' : ''),
      onClick: toggle,
      style: { paddingLeft: indent + 8 + 'px' },
    },
      entry.is_dir
        ? h('span', { className: 'file-tree-arrow' }, expanded ? '▾' : '▸')
        : h('span', { className: 'file-tree-arrow' }),
      h('span', { className: 'file-tree-icon' }, entry.is_dir ? '📁' : '📄'),
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
