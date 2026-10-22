// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import { renderLiteCore } from '@/lib/markdownShared';

function cells(html: string): { tag: string; nowrap: boolean; text: string }[] {
  const doc = new DOMParser().parseFromString(html, 'text/html');
  return [...doc.querySelectorAll('th, td')].map((c) => ({
    tag: c.tagName.toLowerCase(),
    nowrap: c.classList.contains('md-nowrap'),
    text: c.textContent ?? '',
  }));
}

describe('markdown tables', () => {
  it('wraps the table in a scroll container', () => {
    const html = renderLiteCore('| a | b |\n|---|---|\n| 1 | 2 |');
    const doc = new DOMParser().parseFromString(html, 'text/html');
    const wrapper = doc.body.firstElementChild;
    expect(wrapper?.className).toBe('md-table');
    expect(wrapper?.firstElementChild?.tagName).toBe('TABLE');
  });

  it('keeps short cells on one line and lets long ones wrap', () => {
    const long = 'a sentence long enough that it should wrap inside its cell';
    const html = renderLiteCore(`| plan → next | note |\n|---|---|\n| 0 → 0 | ${long} |`);
    expect(cells(html)).toEqual([
      { tag: 'th', nowrap: true, text: 'plan → next' },
      { tag: 'th', nowrap: true, text: 'note' },
      { tag: 'td', nowrap: true, text: '0 → 0' },
      { tag: 'td', nowrap: false, text: long },
    ]);
  });

  it('measures rendered text: CJK counts double, markup does not count', () => {
    const cjk13 = '一二三四五六七八九十一二三'; // width 26
    const link = '[docs](https://example.com/a/very/long/path/that/is/not/visible)';
    const html = renderLiteCore(`| 订阅状态 | ${cjk13} | ${link} | \`a & b\` |\n|---|---|---|---|\n| x | y | z | w |`);
    expect(cells(html).slice(0, 4).map((c) => c.nowrap)).toEqual([true, false, true, true]);
  });

  it('keeps alignment on a nowrap cell', () => {
    const html = renderLiteCore('| n |\n|--:|\n| 3 |');
    expect(html).toContain('<td class="md-nowrap" align="right">3</td>');
  });
});
