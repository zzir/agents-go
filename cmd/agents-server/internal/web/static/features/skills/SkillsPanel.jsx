import React from 'react';
import { api } from '/lib/api.js';

const { useState, useEffect, useCallback } = React;
const h = React.createElement;

export function SkillsPanel() {
  const [skills, setSkills] = useState(null);
  const [loading, setLoading] = useState(true);
  const [repoUrl, setRepoUrl] = useState('');
  const [cloning, setCloning] = useState(false);
  const [error, setError] = useState('');

  const reload = useCallback(() => {
    setLoading(true);
    api.skills.list()
      .then(data => setSkills(data || []))
      .catch(() => setSkills([]))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { reload(); }, [reload]);

  const handleClone = async () => {
    const url = repoUrl.trim();
    if (!url) return;
    setCloning(true);
    setError('');
    try {
      await api.skills.clone(url);
      setRepoUrl('');
      reload();
    } catch (e) {
      setError(e.message || 'Clone failed');
    } finally {
      setCloning(false);
    }
  };

  const [updating, setUpdating] = useState('');

  const handleUpdate = async (topDir) => {
    setUpdating(topDir);
    setError('');
    try {
      await api.skills.update(topDir);
      reload();
    } catch (e) {
      setError(e.message || 'Update failed');
    } finally {
      setUpdating('');
    }
  };

  const handleDelete = async (topDir) => {
    if (!confirm('Delete "' + topDir + '"?')) return;
    try {
      await api.skills.delete(topDir);
      reload();
    } catch (e) {
      setError(e.message || 'Delete failed');
    }
  };

  const grouped = groupByRepo(skills || []);

  return h('div', null,
    h('div', { style: { display: 'flex', gap: '6px', marginBottom: '16px' } },
      h('input', {
        type: 'text',
        className: 'form-control',
        placeholder: 'Git repository URL',
        value: repoUrl,
        onChange: e => setRepoUrl(e.target.value),
        onKeyDown: e => { if (e.key === 'Enter' && !cloning) handleClone(); },
        disabled: cloning,
        style: { flex: 1 },
      }),
      h('button', {
        className: 'btn btn-primary btn-sm',
        onClick: handleClone,
        disabled: cloning || !repoUrl.trim(),
      }, cloning ? 'Cloning...' : '+ Add'),
    ),

    error && h('div', {
      style: { fontSize: '13px', color: 'var(--color-danger-fg)', marginBottom: '12px', wordBreak: 'break-all' },
    }, error),

    loading && h('div', { style: { color: 'var(--color-fg-muted)', fontSize: '13px' } }, 'Scanning...'),

    !loading && grouped.length === 0 && h('div', { style: { color: 'var(--color-fg-muted)', fontSize: '13px' } }, 'No skills installed.'),

    grouped.map(group =>
      h('div', { key: group.repo, className: 'Box', style: { marginBottom: '12px' } },
        h('div', { className: 'Box-row', style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', fontWeight: 600, fontSize: '13px' } },
          group.repo,
          h('span', { style: { display: 'flex', gap: '6px' } },
            h('button', {
              className: 'btn btn-sm',
              onClick: () => handleUpdate(group.repo),
              disabled: updating === group.repo,
              style: { fontSize: '12px' },
            }, updating === group.repo ? 'Updating...' : 'Update'),
            h('button', {
              className: 'btn btn-sm btn-danger',
              onClick: () => handleDelete(group.repo),
              style: { fontSize: '12px' },
            }, 'Delete'),
          ),
        ),
        group.skills.map(s =>
          h('div', { key: s.path, className: 'Box-row', style: { paddingLeft: '24px', fontSize: '13px' } },
            h('span', { style: { fontWeight: 500 } }, s.name),
            s.description && h('span', { style: { color: 'var(--color-fg-muted)', marginLeft: '8px' } }, '— ' + s.description),
          ),
        ),
      ),
    ),
  );
}

function groupByRepo(skills) {
  const map = new Map();
  for (const s of skills) {
    const repo = s.path.includes('/') ? s.path.split('/')[0] : s.path;
    if (!map.has(repo)) map.set(repo, []);
    map.get(repo).push(s);
  }
  return Array.from(map.entries()).map(([repo, skills]) => ({ repo, skills }));
}
