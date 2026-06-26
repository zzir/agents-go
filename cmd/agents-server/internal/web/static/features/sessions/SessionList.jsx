import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState, useEffect } = React;
const h = React.createElement;

// Octicon: bookmark (outline)
function iconBookmark() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 14, height: 14 },
    h('path', { d: 'M3 2.75C3 1.784 3.784 1 4.75 1h6.5c.966 0 1.75.784 1.75 1.75v11.5a.75.75 0 0 1-1.227.579L8 11.722l-3.773 3.107A.75.75 0 0 1 3 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v9.91l3.023-2.489a.75.75 0 0 1 .954 0l3.023 2.49V2.75a.25.25 0 0 0-.25-.25Z' }),
  );
}

// Octicon: bookmark-slash (for unpin)
function iconBookmarkSlash() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 14, height: 14 },
    h('path', { d: 'M3.354.854a.5.5 0 1 0-.708-.708l13 13a.5.5 0 0 0 .708-.708ZM4.75 1h6.5c.966 0 1.75.784 1.75 1.75v11.5a.75.75 0 0 1-1.227.579L8 11.722l-3.773 3.107A.75.75 0 0 1 3 14.25V2.75C3 1.784 3.784 1 4.75 1Zm-.25 1.75v9.91l3.023-2.489a.75.75 0 0 1 .954 0l3.023 2.49V2.75a.25.25 0 0 0-.25-.25h-6.5a.25.25 0 0 0-.25.25Z' }),
  );
}

// Octicon: git-branch
function iconFork() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 14, height: 14 },
    h('path', { d: 'M5 3.254V3.25v.005a.75.75 0 1 1 0-.005Zm.45 1.9a2.25 2.25 0 1 0-1.95.218v5.256a2.25 2.25 0 1 0 1.5 0V7.123A5.735 5.735 0 0 0 9.25 9h1.378a2.251 2.251 0 1 0 0-1.5H9.25a4.25 4.25 0 0 1-3.8-2.346ZM12.75 9a.75.75 0 1 1 0-1.5.75.75 0 0 1 0 1.5Zm-8.5 3.5a.75.75 0 1 1 0-1.5.75.75 0 0 1 0 1.5Z' }),
  );
}

// Octicon: trash
function iconTrash() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 14, height: 14 },
    h('path', { d: 'M11 1.75V3h2.25a.75.75 0 0 1 0 1.5H2.75a.75.75 0 0 1 0-1.5H5V1.75C5 .784 5.784 0 6.75 0h2.5C10.216 0 11 .784 11 1.75ZM6.5 1.75v1.5h3v-1.5a.25.25 0 0 0-.25-.25h-2.5a.25.25 0 0 0-.25.25ZM4.496 6.675l.66 6.6a.25.25 0 0 0 .249.225h5.19a.25.25 0 0 0 .249-.225l.66-6.6a.75.75 0 0 1 1.492.149l-.66 6.6A1.748 1.748 0 0 1 10.595 15h-5.19a1.75 1.75 0 0 1-1.741-1.575l-.66-6.6a.75.75 0 1 1 1.492-.15Z' }),
  );
}

function SessionItem({ s, activeId, isRunning, onSelect, onPin, onFork, onDelete }) {
  return h('div', {
    key: s.id,
    className: 'session-item' + (s.id === activeId ? ' active' : '') + (isRunning ? ' running' : ''),
    'aria-current': s.id === activeId ? 'page' : undefined,
    onClick: () => onSelect(s.id),
  },
    h('span', { className: 'session-item-name' }, s.name),
    h('div', { className: 'session-actions' },
      h('button', {
        className: 'session-action-btn',
        onClick: (e) => { e.stopPropagation(); onPin(s.id, !s.pinned); },
        title: s.pinned ? 'Unpin' : 'Pin',
      }, s.pinned ? iconBookmarkSlash() : iconBookmark()),
      h('button', {
        className: 'session-action-btn',
        onClick: (e) => { e.stopPropagation(); onFork(s.id); },
        title: 'Fork conversation',
      }, iconFork()),
      h('button', {
        className: 'session-action-btn session-action-btn--danger',
        onClick: (e) => { e.stopPropagation(); onDelete(s.id); },
        title: 'Delete',
      }, iconTrash()),
    ),
  );
}

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

  const handleDelete = async (id) => {
    await api.sessions.delete(id);
    reload();
    if (activeId === id) onSelect(null);
  };

  const handleFork = async (id) => {
    const forked = await api.sessions.fork(id);
    await reload();
    onSelect(forked.id);
  };

  const handlePin = async (id, pinned) => {
    await api.sessions.pin(id, pinned);
    reload();
  };

  const pinned = sessions ? sessions.filter(s => s.pinned) : [];
  const recents = sessions ? sessions.filter(s => !s.pinned) : [];

  const renderItem = (s) => h(SessionItem, {
    key: s.id, s, activeId,
    isRunning: runningSessions && runningSessions.has(s.id),
    onSelect, onPin: handlePin, onFork: handleFork, onDelete: handleDelete,
  });

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
      pinned.length > 0 && h('div', { className: 'session-section' },
        h('div', { className: 'session-section-title' }, 'Pinned'),
        pinned.map(renderItem),
      ),
      recents.length > 0 && h('div', { className: 'session-section' },
        (pinned.length > 0) && h('div', { className: 'session-section-title' }, 'Recents'),
        recents.map(renderItem),
      ),
      (!sessions || sessions.length === 0) && h('div', { className: 'blankslate' }, 'No conversations yet'),
    ),
  );
}
