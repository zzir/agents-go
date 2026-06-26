import React from 'react';
import { iconArrowUp as ArrowUpIcon, iconStop as StopIcon } from '/lib/icons.js';

const { useState } = React;
const h = React.createElement;

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
