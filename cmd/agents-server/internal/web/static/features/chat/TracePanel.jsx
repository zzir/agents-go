import React from 'react';

const { useState, useEffect } = React;
const h = React.createElement;

export function TracePanel({ traceRuns, liveRunId }) {
  const [showTrace, setShowTrace] = useState(false);
  const [expandedRuns, setExpandedRuns] = useState({});

  useEffect(() => {
    const runIds = Object.keys(traceRuns);
    if (!runIds.length) return;
    setExpandedRuns(prev => {
      const newRuns = runIds.filter(rid => !(rid in prev));
      if (!newRuns.length) return prev;
      const next = { ...prev };
      newRuns.forEach(rid => { next[rid] = true; });
      return next;
    });
  }, [traceRuns]);

  const runCount = Object.keys(traceRuns).length;
  if (runCount === 0) return null;

  return h('div', { className: 'trace-panel' },
    h('div', {
      className: 'trace-toggle',
      onClick: () => setShowTrace(!showTrace),
    }, (showTrace ? '▾' : '▸') + ' Traces (' + runCount + ' run' + (runCount > 1 ? 's' : '') + ')'),
    showTrace && h('div', { style: { maxHeight: '200px', overflowY: 'auto', marginTop: '4px' } },
      Object.entries(traceRuns).map(([rid, events]) => {
        const isLive = rid === liveRunId;
        const isExpanded = expandedRuns[rid];
        const hookCount = events.filter(e => e.kind === 'hook').length;
        const spanCount = events.filter(e => e.kind === 'span').length;
        return h('div', { key: rid, style: { marginBottom: '4px' } },
          h('div', {
            className: 'trace-run-header',
            onClick: () => setExpandedRuns(prev => ({ ...prev, [rid]: !prev[rid] })),
            style: { cursor: 'pointer', display: 'flex', alignItems: 'center', gap: '6px', fontSize: '11px', padding: '2px 0' },
          },
            h('span', null, isExpanded ? '▾' : '▸'),
            h('span', { style: { fontFamily: 'monospace', color: 'var(--color-fg-muted)' } }, rid.slice(0, 8)),
            isLive && h('span', { className: 'Label Label--accent', style: { fontSize: '10px' } }, 'live'),
            h('span', { style: { color: 'var(--color-fg-subtle)' } },
              hookCount + ' hook' + (hookCount !== 1 ? 's' : '') + ', ' + spanCount + ' span' + (spanCount !== 1 ? 's' : '')),
          ),
          isExpanded && h('div', { style: { paddingLeft: '12px' } },
            events.map((e, i) =>
              h('div', { key: i, className: 'trace-event' },
                h('span', { style: { color: e.kind === 'hook' ? 'var(--color-accent-fg)' : 'var(--color-success-fg)' } },
                  e.kind === 'hook' ? e.name : '◆ ' + e.name),
                e.detail && h('span', { style: { color: 'var(--color-fg-subtle)', marginLeft: '6px' } }, e.detail),
              ),
            ),
          ),
        );
      }),
    ),
  );
}
