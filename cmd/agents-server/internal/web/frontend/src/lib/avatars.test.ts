import { describe, expect, it } from 'vitest';
import { AVATARS } from './avatars';

// The catalog and public/avatars must list exactly the same files: a file the
// catalog misses is unpickable, a catalog entry with no file is a broken image.
describe('avatar catalog', () => {
  it('matches public/avatars exactly', () => {
    const files = Object.keys(import.meta.glob('../../public/avatars/*.svg'))
      .map(p => p.split('/').pop()!).sort();
    const catalog = AVATARS.map(a => a.path.replace('/avatars/', '')).sort();
    expect(files.length).toBeGreaterThan(0);
    expect(catalog).toEqual(files);
  });

  it('has a label per entry', () => {
    for (const a of AVATARS) expect(a.label).not.toBe('');
  });
});
