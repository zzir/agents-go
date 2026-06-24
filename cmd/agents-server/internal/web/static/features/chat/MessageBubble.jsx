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
    h('div', {
      className: 'message-body markdown-body',
      dangerouslySetInnerHTML: { __html: html },
    }),
  );
}
