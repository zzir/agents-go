import React from 'react';
import { api } from '/lib/api.js';
import { iconChevronSmall, iconDir, iconFile } from '/lib/icons.js';

const { useState, useEffect, useCallback } = React;
const h = React.createElement;

function ChevronIcon({ expanded }) {
  return iconChevronSmall({ className: 'file-tree-chevron' + (expanded ? ' expanded' : '') });
}

function DirIcon() {
  return iconDir({ className: 'file-tree-icon' });
}

function FileIcon() {
  return iconFile({ className: 'file-tree-icon' });
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
      className: 'file-tree-node' + (isSelected ? ' active' : '') + (entry.is_dir ? ' is-dir' : ''),
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
