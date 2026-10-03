import { describe, it, expect } from 'vitest';
import { latestChecklist, parseChecklist } from '@/lib/checklist';
import type { TimelineEntry } from '@/lib/timeline';

const call = (id: string, args: string, extra: Record<string, unknown> = {}) =>
  ({ tool_call_id: id, tool_name: 'todo_write', arguments: args, output: 'ok', status: 'completed', ...extra });
const turn = (...toolCalls: ReturnType<typeof call>[]): TimelineEntry =>
  ({ role: 'turn', messageId: '', parts: [{ type: 'tools', toolCalls }] } as unknown as TimelineEntry);
const two = '{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}';
const three = '{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"completed"},{"content":"c","status":"pending"}]}';

describe('checklist', () => {
  it('reads the list and counts what is done', () => {
    expect(parseChecklist(two)).toEqual([{ content: 'a', status: 'completed' }, { content: 'b', status: 'in_progress' }]);
    expect(parseChecklist('{"plan":"x"}')).toBeNull();
    expect(parseChecklist('not json')).toBeNull();
  });

  // The newest call is the list; an older card's "1/2 done" is history.
  it('picks the newest accepted call', () => {
    const got = latestChecklist([turn(call('c1', two)), { role: 'user', content: 'go' } as TimelineEntry, turn(call('c2', three))]);
    expect(got).toEqual({ callId: 'c2', done: 2, items: parseChecklist(three) });
  });

  it('skips a call that changed nothing', () => {
    const got = latestChecklist([turn(call('c1', two)), turn(call('c2', three, { status: 'rejected' }), call('c3', three, { not_run: 'stopped' }))]);
    expect(got?.callId).toBe('c1');
    expect(latestChecklist([])).toBeNull();
  });
});
