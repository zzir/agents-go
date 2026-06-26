import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';
import { iconBookmark, iconBookmarkSlash, iconFork, iconTrash } from '/lib/icons.js';

const { useState, useEffect } = React;
const h = React.createElement;

function SessionItem({ s, activeId, isRunning, onSelect, onPin, onFork, onDelete }) {
  return h('a', {
    key: s.id,
    className: 'NavList-item' + (isRunning ? ' running' : ''),
    'aria-current': s.id === activeId ? 'page' : undefined,
    onClick: (e) => { e.preventDefault(); onSelect(s.id); },
    href: '#',
  },
    h('span', { className: 'NavList-item-label' }, s.name),
    h('div', { className: 'NavList-item-actions' },
      h('button', {
        className: 'btn-octicon',
        onClick: (e) => { e.stopPropagation(); e.preventDefault(); onPin(s.id, !s.pinned); },
        title: s.pinned ? 'Unpin' : 'Pin',
      }, s.pinned ? iconBookmarkSlash() : iconBookmark()),
      h('button', {
        className: 'btn-octicon',
        onClick: (e) => { e.stopPropagation(); e.preventDefault(); onFork(s.id); },
        title: 'Fork conversation',
      }, iconFork()),
      h('button', {
        className: 'btn-octicon NavList-item-action--danger',
        onClick: (e) => { e.stopPropagation(); e.preventDefault(); onDelete(s.id); },
        title: 'Delete',
      }, iconTrash()),
    ),
  );
}

export function SessionList({ activeId, onSelect, onDelete: onDeleteNotify, onCreated, reloadKey, runningSessions }) {
  const { data: sessions, reload } = useApi(() => api.sessions.list());

  useEffect(() => {
    if (reloadKey) reload();
  }, [reloadKey, reload]);
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    setCreating(true);
    try {
      const sess = await api.sessions.create('New Chat');
      await reload();
      onSelect(sess.id);
      if (onCreated) onCreated();
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async (id) => {
    await api.sessions.delete(id);
    reload();
    if (onDeleteNotify) onDeleteNotify(id);
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
      h('nav', { className: 'NavList' },
        pinned.length > 0 && h('div', { className: 'NavList-group-title' }, 'Pinned'),
        pinned.map(renderItem),
        recents.length > 0 && (pinned.length > 0) && h('div', { className: 'NavList-group-title' }, 'Recents'),
        recents.map(renderItem),
      ),
      (!sessions || sessions.length === 0) && h('div', { className: 'blankslate' }, 'No conversations yet'),
    ),
  );
}
