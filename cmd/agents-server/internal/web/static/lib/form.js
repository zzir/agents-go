import React from 'react';

const h = React.createElement;

export function fc(label, input, hint) {
  return h('div', { className: 'FormControl' },
    label && h('label', { className: 'FormControl-label' }, label),
    input,
    hint && h('div', { className: 'FormControl-caption' }, hint),
  );
}
