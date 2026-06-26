import React from 'react';
import { api } from '/lib/api.js';
import { hljs } from '/lib/markdown.js';
import { iconFileBadge, iconAlert } from '/lib/icons.js';

const { useState, useEffect, useMemo } = React;
const h = React.createElement;

const EXT_MAP = {
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  ts: 'typescript', tsx: 'typescript',
  py: 'python', go: 'go', rs: 'rust',
  java: 'java', c: 'c', h: 'c', cpp: 'cpp', cc: 'cpp', cxx: 'cpp', hpp: 'cpp',
  cs: 'csharp', swift: 'swift', kt: 'kotlin', kts: 'kotlin',
  rb: 'ruby', php: 'php', lua: 'lua', pl: 'perl', r: 'r', scala: 'scala',
  sh: 'bash', bash: 'bash', zsh: 'bash',
  sql: 'sql', json: 'json', yaml: 'yaml', yml: 'yaml',
  xml: 'xml', html: 'xml', htm: 'xml', svg: 'xml',
  css: 'css', scss: 'scss', sass: 'scss',
  md: 'markdown', markdown: 'markdown',
  dockerfile: 'dockerfile', makefile: 'makefile',
  toml: 'ini', ini: 'ini', conf: 'ini', cfg: 'ini',
  properties: 'properties', env: 'properties',
  diff: 'diff', patch: 'diff',
  proto: 'protobuf', graphql: 'graphql', gql: 'graphql',
  nginx: 'nginx',
};

const BINARY_EXTS = new Set([
  'db', 'sqlite', 'sqlite3', 'exe', 'dll', 'so', 'dylib', 'bin', 'dat',
  'zip', 'gz', 'tar', 'bz2', 'xz', '7z', 'rar', 'zst',
  'jpg', 'jpeg', 'png', 'gif', 'bmp', 'ico', 'webp', 'avif', 'tiff',
  'mp3', 'mp4', 'avi', 'mov', 'wav', 'flac', 'ogg', 'webm', 'mkv',
  'pdf', 'doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx',
  'woff', 'woff2', 'ttf', 'otf', 'eot',
  'class', 'pyc', 'pyo', 'o', 'a', 'lib', 'obj',
  'wasm', 'jar', 'war', 'ear',
]);

function isBinary(filePath) {
  if (!filePath) return false;
  const name = filePath.split('/').pop().toLowerCase();
  const ext = name.includes('.') ? name.split('.').pop() : '';
  return BINARY_EXTS.has(ext);
}

function getLang(filePath) {
  if (!filePath) return null;
  const name = filePath.split('/').pop().toLowerCase();
  if (name === 'dockerfile' || name.startsWith('dockerfile.')) return 'dockerfile';
  if (name === 'makefile' || name === 'gnumakefile') return 'makefile';
  if (name === '.env' || name.startsWith('.env.')) return 'properties';
  if (name === 'nginx.conf') return 'nginx';
  const ext = name.split('.').pop();
  return EXT_MAP[ext] || null;
}

export function FileViewer({ filePath }) {
  const [content, setContent] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);

  useEffect(() => {
    if (!filePath || isBinary(filePath)) { setContent(null); return; }
    setLoading(true); setError(null);
    api.files.read(filePath)
      .then(r => setContent(r.content || ''))
      .catch(e => setError(e.message))
      .finally(() => setLoading(false));
  }, [filePath]);

  const rendered = useMemo(() => {
    if (content == null) return null;
    const lang = getLang(filePath);
    let lines;
    if (lang && hljs.getLanguage(lang)) {
      const raw = hljs.highlight(content, { language: lang }).value;
      lines = raw.split('\n');
    } else {
      lines = (content || '').split('\n').map(l => l.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'));
    }
    return lines.map((line, i) =>
      `<span class="code-line" data-ln="${i + 1}">${line || ' '}</span>`
    ).join('');
  }, [content, filePath]);

  if (!filePath) {
    return h('div', { className: 'chat-empty' },
      h('div', { className: 'chat-empty-badge' }, iconFileBadge()),
      h('div', { className: 'chat-empty-title' }, 'No file selected'),
      h('div', { className: 'chat-empty-sub' }, 'Choose a file from the tree to view its contents.'),
    );
  }

  if (loading) {
    return h('div', { style: { padding: '24px', color: 'var(--color-fg-muted)' } }, 'Loading...');
  }

  const alertIcon = iconAlert();

  if (isBinary(filePath) || error) {
    const title = isBinary(filePath) ? 'Binary file' : 'Unable to display';
    const sub = isBinary(filePath) ? 'This file type cannot be displayed as text.' : error;
    return h('div', { style: { display: 'flex', flexDirection: 'column', height: '100%' } },
      h('div', { className: 'file-viewer-header' },
        h('span', { className: 'file-viewer-path' }, filePath),
      ),
      h('div', { className: 'chat-empty' },
        h('div', { className: 'chat-empty-badge' }, alertIcon),
        h('div', { className: 'chat-empty-title' }, title),
        h('div', { className: 'chat-empty-sub' }, sub),
      ),
    );
  }

  return h('div', { style: { display: 'flex', flexDirection: 'column', height: '100%' } },
    h('div', { className: 'file-viewer-header' },
      h('span', { className: 'file-viewer-path' }, filePath),
    ),
    h('pre', { className: 'file-viewer-content has-line-numbers' },
      h('code', { className: 'hljs', dangerouslySetInnerHTML: { __html: rendered } }),
    ),
  );
}
