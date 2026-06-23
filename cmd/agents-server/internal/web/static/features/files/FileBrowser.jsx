import React from 'react';
import { api } from '/lib/api.js';

const { useState, useEffect } = React;
const h = React.createElement;

export function FileBrowser() {
  const [path, setPath] = useState('');
  const [entries, setEntries] = useState([]);
  const [fileContent, setFileContent] = useState(null);
  const [fileName, setFileName] = useState('');
  const [loading, setLoading] = useState(false);

  const loadDir = async (p) => {
    setLoading(true); setFileContent(null); setFileName('');
    try { const items = await api.files.list(p); setEntries(items || []); setPath(p); }
    catch (e) { setEntries([]); }
    setLoading(false);
  };

  const loadFile = async (p) => {
    setLoading(true);
    try { const result = await api.files.read(p); setFileContent(result.content || ''); setFileName(p); }
    catch (e) { setFileContent('Error: ' + e.message); setFileName(p); }
    setLoading(false);
  };

  useEffect(() => { loadDir(''); }, []);

  const handleClick = (entry) => {
    const fullPath = path ? path + '/' + entry.name : entry.name;
    if (entry.is_dir) loadDir(fullPath);
    else loadFile(fullPath);
  };

  const goUp = () => {
    const parts = path.split('/').filter(Boolean);
    parts.pop();
    loadDir(parts.join('/'));
  };

  const breadcrumbs = ['root', ...path.split('/').filter(Boolean)];

  return h('div', { style: { display: 'flex', flexDirection: 'column', height: '100%' } },
    h('div', { style: { display: 'flex', alignItems: 'center', gap: '4px', marginBottom: '12px', flexWrap: 'wrap' } },
      breadcrumbs.map((part, i) =>
        h(React.Fragment, { key: i },
          i > 0 && h('span', { style: { color: 'var(--color-fg-subtle)', fontSize: '13px' } }, '/'),
          h('span', {
            onClick: () => { if (i === 0) loadDir(''); else loadDir(breadcrumbs.slice(1, i + 1).join('/')); },
            style: { cursor: 'pointer', fontSize: '13px', fontWeight: i === breadcrumbs.length - 1 ? 600 : 400, color: 'var(--color-accent-fg)' },
          }, part),
        ),
      ),
    ),
    fileContent !== null
      ? h('div', { style: { flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' } },
          h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' } },
            h('span', { style: { fontSize: '13px', fontWeight: 500, fontFamily: 'var(--font-mono)' } }, fileName),
            h('button', { onClick: () => { setFileContent(null); setFileName(''); }, className: 'btn btn-sm' }, 'Back'),
          ),
          h('pre', {
            style: { flex: 1, overflow: 'auto', padding: '12px', margin: 0, border: '1px solid var(--color-border-default)', borderRadius: 'var(--radius-md)', background: 'var(--color-canvas-subtle)', fontSize: '12px', fontFamily: 'var(--font-mono)', color: 'var(--color-fg-default)', whiteSpace: 'pre-wrap', wordBreak: 'break-all' },
          }, fileContent),
        )
      : h('div', { className: 'Box', style: { flex: 1, overflow: 'auto' } },
          path && h('div', { className: 'Box-row', onClick: goUp, style: { cursor: 'pointer', color: 'var(--color-fg-muted)', fontSize: '13px' } }, '..'),
          loading
            ? h('div', { style: { padding: '12px', color: 'var(--color-fg-muted)', fontSize: '13px' } }, 'Loading...')
            : entries.map(e =>
                h('div', { key: e.name, className: 'Box-row', onClick: () => handleClick(e), style: { cursor: 'pointer', gap: '8px' } },
                  h('span', { style: { width: '16px', textAlign: 'center', fontSize: '13px', color: 'var(--color-fg-muted)' } }, e.is_dir ? '\u{1F4C1}' : '\u{1F4C4}'),
                  h('span', { style: { flex: 1, fontSize: '13px' } }, e.name),
                  !e.is_dir && h('span', { style: { fontSize: '11px', color: 'var(--color-fg-subtle)' } }, formatSize(e.size)),
                ),
              ),
        ),
  );
}

function formatSize(bytes) {
  if (!bytes) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}
