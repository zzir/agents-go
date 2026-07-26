import { describe, expect, it } from 'vitest';
import { parseTaskNotification, TASK_NOTIFICATION_PREFIX } from './protocol';

// These strings are what the SDK's tasks.DefaultNotifyFormatter emits. If a
// change there breaks these, the UI stops recognizing notifications — which is
// exactly the drift that had the format defined in Go and re-derived here.
describe('parseTaskNotification', () => {
  it('parses one wake-up carrying several tasks', () => {
    const msg =
      TASK_NOTIFICATION_PREFIX +
      'Task "index the docs" (a1b2c3) completed. Result: indexed 412 files\n' +
      'Task "check links" (d4e5f6) failed. Result: 3 dead links';

    const got = parseTaskNotification(msg);
    expect(got).not.toBeNull();
    expect(got!.items).toHaveLength(2);
    expect(got!.items[0]).toEqual({
      label: 'index the docs',
      taskId: 'a1b2c3',
      status: 'completed',
      summary: 'indexed 412 files',
      truncated: false,
    });
    expect(got!.items[1].status).toBe('failed');
    // The first task drives the collapsed card's title.
    expect(got!.label).toBe('index the docs');
    expect(got!.taskId).toBe('a1b2c3');
  });

  it('reports truncation and strips its marker', () => {
    const msg =
      TASK_NOTIFICATION_PREFIX +
      'Task "big job" (xyz) completed. Result: the first part… ' +
      '[truncated — call task_status(xyz) for the full result]';

    const got = parseTaskNotification(msg);
    expect(got!.items[0].truncated).toBe(true);
    expect(got!.items[0].summary).toBe('the first part…');
  });

  // The label is Go-quoted, so it can contain escaped quotes; the naive
  // `"([^"]+)"` would stop at the first one and drop the rest of the line.
  it('handles a label containing quotes', () => {
    const msg = TASK_NOTIFICATION_PREFIX + 'Task "the \\"big\\" job" (id1) completed.';
    const got = parseTaskNotification(msg);
    expect(got!.items).toHaveLength(1);
    expect(got!.items[0].label).toBe('the "big" job');
    expect(got!.items[0].taskId).toBe('id1');
  });

  // The host mints task ids; they are not necessarily hex.
  it('accepts a non-hex task id', () => {
    const got = parseTaskNotification(TASK_NOTIFICATION_PREFIX + 'Task "x" (task_2024-ZZ) cancelled.');
    expect(got!.items[0].taskId).toBe('task_2024-ZZ');
  });

  it('reports no summary when the task had none', () => {
    const got = parseTaskNotification(TASK_NOTIFICATION_PREFIX + 'Task "quiet" (q1) cancelled.');
    expect(got!.items[0].summary).toBe('');
    expect(got!.items[0].truncated).toBe(false);
  });

  it('ignores anything that is not a notification', () => {
    for (const s of ['hello', '', null, undefined, 'Task "x" (id) completed.']) {
      expect(parseTaskNotification(s)).toBeNull();
    }
  });
});
