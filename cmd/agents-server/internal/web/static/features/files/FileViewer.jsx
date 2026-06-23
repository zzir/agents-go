import React from 'react';
import { api } from '/lib/api.js';

const { useState, useEffect } = React;
const h = React.createElement;

export function FileViewer({ filePath }) {
  const [content, setContent] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    if (!filePath) { setContent(null); return; }
    setLoading(true); setError(null);
    api.files.read(filePath)
      .then(r => setContent(r.content || ''))
      .catch(e => setError(e.message))
      .finally(() => setLoading(false));
  }, [filePath]);

  if (!filePath) {
    return h('div', { className: 'chat-empty' },
      h('div', { className: 'chat-empty-badge' },
        h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', 'aria-hidden': 'true' },
          h('path', { d: 'M2 1.75C2 .784 2.784 0 3.75 0h6.586c.464 0 .909.184 1.237.513l2.914 2.914c.329.328.513.773.513 1.237v9.586A1.75 1.75 0 0 1 13.25 16h-9.5A1.75 1.75 0 0 1 2 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 0 0 .25-.25V6h-2.75A1.75 1.75 0 0 1 9 4.25V1.5Zm6.75.062V4.25c0 .138.112.25.25.25h2.688l-.011-.013-2.914-2.914-.013-.011Z' }),
        ),
      ),
      h('div', { className: 'chat-empty-title' }, 'No file selected'),
      h('div', { className: 'chat-empty-sub' }, 'Choose a file from the tree to view its contents.'),
    );
  }

  if (loading) {
    return h('div', { style: { padding: '24px', color: 'var(--color-fg-muted)' } }, 'Loading...');
  }

  return h('div', { style: { display: 'flex', flexDirection: 'column', height: '100%' } },
    h('div', { className: 'file-viewer-header' },
      h('span', { className: 'file-viewer-path' }, filePath),
    ),
    error
      ? h('div', { style: { padding: '16px', color: 'var(--color-danger-fg)' } }, error)
      : h('pre', { className: 'file-viewer-content' }, content),
  );
}
