import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { Disclosure } from './Disclosure';

// No DOM environment in this test setup: the static markup pins the ARIA
// wiring; click and keyboard toggling stay a browser concern.
const header = (html: string) => html.match(/<(button|div)[^>]*class="disclosure-header"[^>]*>/)?.[0] ?? '';
const attr = (el: string, name: string) => el.match(new RegExp(' ' + name + '="([^"]*)"'))?.[1];
const bodyId = (html: string) => html.match(/<div id="([^"]*)" class="disclosure-body">/)?.[1];

describe('Disclosure ARIA contract', () => {
  it('collapsed: a type=button header with aria-expanded=false and no body', () => {
    const html = renderToStaticMarkup(<Disclosure label="x">body</Disclosure>);
    const h = header(html);
    expect(h.startsWith('<button')).toBe(true);
    expect(attr(h, 'type')).toBe('button');
    expect(attr(h, 'aria-expanded')).toBe('false');
    expect(attr(h, 'aria-controls')).toBeTruthy();
    expect(html).not.toContain('disclosure-body');
  });

  it('expanded via defaultOpen / open / forceOpen: aria-expanded=true, aria-controls names the body', () => {
    for (const props of [{ defaultOpen: true }, { open: true }, { forceOpen: true }]) {
      const html = renderToStaticMarkup(<Disclosure label="x" {...props}>body</Disclosure>);
      const h = header(html);
      expect(attr(h, 'aria-expanded')).toBe('true');
      expect(bodyId(html)).toBeTruthy();
      expect(attr(h, 'aria-controls')).toBe(bodyId(html));
    }
  });

  it('as="div": the header is a focusable role=button', () => {
    const html = renderToStaticMarkup(<Disclosure as="div" label="x" defaultOpen>body</Disclosure>);
    const h = header(html);
    expect(h.startsWith('<div')).toBe(true);
    expect(attr(h, 'role')).toBe('button');
    expect(attr(h, 'tabindex')).toBe('0');
    expect(attr(h, 'aria-expanded')).toBe('true');
    expect(attr(h, 'aria-controls')).toBe(bodyId(html));
  });
});
