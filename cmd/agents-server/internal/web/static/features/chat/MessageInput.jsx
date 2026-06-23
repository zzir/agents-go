import React from 'react';

const { useState } = React;
const h = React.createElement;

const ArrowUpIcon = () => h('svg', { width: 16, height: 16, viewBox: '0 0 16 16', fill: 'currentColor' },
  h('path', { d: 'M3.47 7.78a.75.75 0 0 1 0-1.06l4-4a.75.75 0 0 1 1.06 0l4 4a.75.75 0 0 1-1.06 1.06L8.75 5.06V13a.75.75 0 0 1-1.5 0V5.06L4.53 7.78a.75.75 0 0 1-1.06 0Z' }),
);

const StopIcon = () => h('svg', { width: 14, height: 14, viewBox: '0 0 16 16', fill: 'currentColor' },
  h('rect', { x: 3, y: 3, width: 10, height: 10, rx: 2 }),
);

export function MessageInput({ onSend, onCancel, disabled, running, footer }) {
  const [text, setText] = useState('');

  const handleSubmit = (e) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText('');
  };

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      handleSubmit(e);
    }
  };

  return h('div', { className: 'chat-input-container' },
    h('form', { onSubmit: handleSubmit, className: 'chat-input-box' },
      h('textarea', {
        value: text,
        onChange: (e) => setText(e.target.value),
        onKeyDown: handleKeyDown,
        placeholder: 'Type a message...',
        rows: 1,
      }),
      running
        ? h('button', {
            type: 'button',
            onClick: onCancel,
            className: 'chat-input-btn chat-input-btn-stop',
            title: 'Stop',
          }, h(StopIcon))
        : h('button', {
            type: 'submit',
            disabled: disabled || !text.trim(),
            className: 'chat-input-btn chat-input-btn-send',
            title: 'Send',
          }, h(ArrowUpIcon)),
    ),
    footer,
  );
}
