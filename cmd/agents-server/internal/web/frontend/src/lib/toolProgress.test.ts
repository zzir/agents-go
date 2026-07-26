import { describe, it, expect } from 'vitest';
import type { TimelineEntry, TurnEntry, ToolsPart } from '@/lib/timeline';
import {
  ensureLiveTurn, appendToolCall, applyToolResult, appendToolProgress,
} from '@/lib/streamReducer';

const RUN = 'run-1';

function withCall(): TimelineEntry[] {
  const live = ensureLiveTurn([], RUN)!;
  return appendToolCall(live, {
    tool_call_id: 'c1', tool_name: 'exec_command',
    arguments: '{"cmd":"make"}', output: null, status: null,
  }, '')!;
}

function call(msgs: TimelineEntry[]) {
  const turn = msgs[msgs.length - 1] as TurnEntry;
  const part = turn.parts.find(p => p.type === 'tools') as ToolsPart;
  return part.toolCalls[0];
}

describe('tool progress', () => {
  // The wire carries deltas: a command producing output over two minutes sends
  // many, and each is the next piece rather than the whole picture.
  it('accumulates deltas', () => {
    let msgs = withCall();
    msgs = appendToolProgress(msgs, 'c1', 'compiling…\n', 'terminal')!;
    msgs = appendToolProgress(msgs, 'c1', 'linking…\n')!;
    expect(call(msgs).progress).toBe('compiling…\nlinking…\n');
    // The renderer hint from the first delta sticks.
    expect(call(msgs).renderer).toBe('terminal');
  });

  // Progress is how the tool got there; the result is the answer. Showing both
  // would show the same work twice.
  it('is cleared when the result lands', () => {
    let msgs = withCall();
    msgs = appendToolProgress(msgs, 'c1', 'working…')!;
    msgs = applyToolResult(msgs, 'c1', 'exit_code: 0')!;
    expect(call(msgs).output).toBe('exit_code: 0');
    expect(call(msgs).progress).toBe('');
  });

  // A goroutine the tool left behind must not reopen a finished call.
  it('ignores a delta that arrives after the result', () => {
    let msgs = withCall();
    msgs = applyToolResult(msgs, 'c1', 'done')!;
    expect(appendToolProgress(msgs, 'c1', 'too late')).toBeNull();
    expect(call(msgs).output).toBe('done');
  });

  it('ignores an empty delta and an unknown call', () => {
    const msgs = withCall();
    expect(appendToolProgress(msgs, 'c1', '')).toBeNull();
    expect(appendToolProgress(msgs, 'nope', 'x')).toBeNull();
  });

  // The streamed and replayed timelines must be identical, so a call that
  // never streamed progress must not carry the key at all.
  it('does not add the key to a call that had no progress', () => {
    let msgs = withCall();
    msgs = applyToolResult(msgs, 'c1', 'done')!;
    expect('progress' in call(msgs)).toBe(false);
  });
});
