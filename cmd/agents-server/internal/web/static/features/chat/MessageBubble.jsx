import React from 'react';
import { renderMarkdown } from '/lib/markdown.js';

const { useMemo } = React;
const h = React.createElement;

export function MessageBubble({ role, content }) {
  const isUser = role === 'user';
  const isSystem = role === 'system';

  const html = useMemo(() => {
    if (isUser || isSystem) return null;
    return renderMarkdown(content);
  }, [content, isUser, isSystem]);

  if (isSystem) {
    return h('div', { className: 'message message-system' },
      h('span', { className: 'message-badge' }, content),
    );
  }

  if (isUser) {
    return h('div', { className: 'message message-user' },
      h('div', { className: 'message-body' }, content),
    );
  }

  return h('div', { className: 'message message-assistant' },
    h('div', { className: 'message-avatar' }, botIcon()),
    h('div', {
      className: 'message-body markdown-body',
      dangerouslySetInnerHTML: { __html: html },
    }),
  );
}

function botIcon() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 16, height: 16 },
    h('path', { d: 'M8 0a1 1 0 0 1 1 1v1.1A5.002 5.002 0 0 1 13 7v2.07a.75.75 0 0 1-.22.53l-.78.78v1.87a.75.75 0 0 1-.75.75h-1.5v1.25a.75.75 0 0 1-1.5 0V13h-1.5v1.25a.75.75 0 0 1-1.5 0V13h-1.5a.75.75 0 0 1-.75-.75v-1.87l-.78-.78A.75.75 0 0 1 3 9.07V7a5.002 5.002 0 0 1 4-4.9V1a1 1 0 0 1 1-1ZM5.5 7.5a1 1 0 1 0 0 2 1 1 0 0 0 0-2Zm5 0a1 1 0 1 0 0 2 1 1 0 0 0 0-2Z' }),
  );
}
