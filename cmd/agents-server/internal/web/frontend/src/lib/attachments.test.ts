import { describe, expect, it } from 'vitest';
import { imageAffordance } from './attachments';

const on = { enabled: true, max_bytes: 1, max_count: 1, downscale_px: 1 };
const off = { ...on, enabled: false };

describe('imageAffordance', () => {
  it('is taken only with storage configured and Vision on', () => {
    expect(imageAffordance(on, true)).toEqual({ enabled: true, hint: '' });
  });
  it('names the missing switch, storage before Vision', () => {
    expect(imageAffordance(off, true)).toEqual({ enabled: false, hint: 'not configured on this server' });
    expect(imageAffordance(off, false).hint).toBe('not configured on this server');
    expect(imageAffordance(on, false)).toEqual({ enabled: false, hint: 'enable Vision on the agent' });
  });
  it('is disabled without a hint while the config loads', () => {
    expect(imageAffordance(null, true)).toEqual({ enabled: false, hint: '' });
  });
});
