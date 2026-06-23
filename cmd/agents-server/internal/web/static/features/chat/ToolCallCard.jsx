import React from 'react';

const h = React.createElement;

export function ToolCallCard({ toolCall, onApprove, onReject }) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output } = toolCall;

  let parsedArgs = args;
  try { parsedArgs = JSON.stringify(JSON.parse(args), null, 2); } catch (e) {}

  const showStatus = status === 'approved' || status === 'rejected' || (needs_approval && !status);
  const statusLabel = status === 'approved' ? 'approved' : status === 'rejected' ? 'rejected' : 'pending';
  const statusClass = status === 'approved' ? 'Label-success' : status === 'rejected' ? 'Label-danger' : 'Label-accent';

  return h('div', { className: 'ToolCallCard' },
    h('div', { className: 'ToolCallCard-header' },
      h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
        h('span', { style: { fontWeight: 600, fontSize: '13px', fontFamily: 'var(--font-sans)' } }, tool_name),
        showStatus && h('span', { className: 'Label ' + statusClass }, statusLabel),
      ),
    ),
    h('div', { className: 'ToolCallCard-body' },
      h('pre', null, parsedArgs),
      output && h('div', { style: { marginTop: '8px' } },
        h('div', { style: { fontSize: '11px', color: 'var(--color-fg-muted)', marginBottom: '4px' } }, 'Output:'),
        h('pre', { style: { color: 'var(--color-fg-default)' } }, output),
      ),
      needs_approval && !status && h('div', { style: { display: 'flex', gap: '8px', marginTop: '10px' } },
        h('button', {
          className: 'btn btn-sm',
          style: { color: 'var(--color-success-fg)', borderColor: 'var(--color-success-fg)' },
          onClick: () => onApprove && onApprove(tool_call_id),
        }, 'Approve'),
        h('button', {
          className: 'btn btn-sm',
          style: { color: 'var(--color-danger-fg)', borderColor: 'var(--color-danger-fg)' },
          onClick: () => onReject && onReject(tool_call_id),
        }, 'Reject'),
      ),
    ),
  );
}
