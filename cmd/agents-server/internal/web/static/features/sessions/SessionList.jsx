import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState, useEffect } = React;
const h = React.createElement;

export function SessionList({ activeId, onSelect, reloadKey, runningSessions }) {
  const { data: sessions, reload } = useApi(() => api.sessions.list());

  useEffect(() => {
    if (reloadKey) reload();
  }, [reloadKey]);
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    setCreating(true);
    try {
      const sess = await api.sessions.create('New Chat');
      await reload();
      onSelect(sess.id);
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async (e, id) => {
    e.stopPropagation();
    await api.sessions.delete(id);
    reload();
    if (activeId === id) onSelect(null);
  };

  return h('div', { style: { display: 'flex', flexDirection: 'column', height: '100%' } },
    h('div', { className: 'chat-pane-header' },
      h('button', {
        onClick: handleCreate,
        disabled: creating,
        className: 'btn btn-sm',
        style: { width: '100%' },
      }, '+ New Chat'),
    ),
    h('div', { className: 'chat-pane-body' },
      sessions && sessions.map(s => {
        const isRunning = runningSessions && runningSessions.has(s.id);
        return h('div', {
          key: s.id,
          className: 'session-item' + (s.id === activeId ? ' active' : '') + (isRunning ? ' running' : ''),
          'aria-current': s.id === activeId ? 'page' : undefined,
          onClick: () => onSelect(s.id),
        },
          h('span', { style: { flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } }, s.name),
          h('button', {
            className: 'delete-btn',
            onClick: (e) => handleDelete(e, s.id),
            title: 'Delete',
          }, '×'),
        );
      }),
      (!sessions || sessions.length === 0) && h('div', { className: 'blankslate' }, 'No conversations yet'),
    ),
  );
}
