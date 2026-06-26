export function buildTimeline(msgs) {
  if (!msgs) return [];
  const timeline = [];
  const pendingTC = {};
  let turn = null;
  const ensureTurn = () => {
    if (!turn) { turn = { role: 'turn', parts: [], messageId: 0 }; timeline.push(turn); }
  };
  const finishTurn = () => { turn = null; };
  for (const m of msgs) {
    if (m.role === 'user') {
      finishTurn();
      if (m.content) timeline.push({ role: 'user', content: m.content, messageId: m.id });
    } else if (m.role === 'tool_call') {
      try {
        const item = JSON.parse(m.item);
        ensureTurn();
        if (m.id) turn.messageId = m.id;
        const tc = { tool_call_id: item.call_id, tool_name: item.name, arguments: item.arguments || '', output: null, status: null };
        pendingTC[item.call_id] = tc;
        const last = turn.parts[turn.parts.length - 1];
        if (last && last.type === 'tools') { last.toolCalls.push(tc); }
        else { turn.parts.push({ type: 'tools', toolCalls: [tc] }); }
      } catch (_) {}
    } else if (m.role === 'tool_output') {
      try {
        const item = JSON.parse(m.item);
        if (turn && m.id) turn.messageId = m.id;
        if (pendingTC[item.call_id]) {
          pendingTC[item.call_id].output = item.output || m.content;
          pendingTC[item.call_id].status = 'completed';
        }
      } catch (_) {}
    } else if (m.role === 'system' && m.content) {
      finishTurn();
      timeline.push({ role: 'system', content: m.content, messageId: m.id });
    } else if (m.content) {
      ensureTurn();
      if (m.id) turn.messageId = m.id;
      turn.parts.push({ type: 'text', content: m.content });
    }
  }
  finishTurn();
  return timeline;
}

export function formatHookDetail(ev) {
  const parts = [];
  if (ev.agent_name) parts.push(ev.agent_name);
  if (ev.tool_name) parts.push('→ ' + ev.tool_name);
  if (ev.from && ev.to) parts.push(ev.from + ' → ' + ev.to);
  if (ev.detail) parts.push(ev.detail);
  return parts.join(' ');
}

export function patchToolCall(messages, toolCallId, patch) {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== 'turn') continue;
    const parts = [...messages[i].parts];
    for (let j = parts.length - 1; j >= 0; j--) {
      if (parts[j].type !== 'tools') continue;
      const tcs = parts[j].toolCalls;
      const idx = tcs.findIndex(tc => tc.tool_call_id === toolCallId);
      if (idx >= 0) {
        const newTcs = [...tcs];
        newTcs[idx] = { ...newTcs[idx], ...patch };
        parts[j] = { ...parts[j], toolCalls: newTcs };
        const newMsgs = [...messages];
        newMsgs[i] = { ...messages[i], parts };
        return newMsgs;
      }
    }
  }
  return null;
}
