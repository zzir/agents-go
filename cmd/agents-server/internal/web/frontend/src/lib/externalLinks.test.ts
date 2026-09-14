// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installExternalLinkOpener } from '@/lib/externalLinks';

describe('installExternalLinkOpener', () => {
  let uninstall: () => void;
  let open: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    open = vi.fn();
    window.open = open as unknown as typeof window.open;
    uninstall = installExternalLinkOpener();
  });
  afterEach(() => { uninstall(); document.body.innerHTML = ''; });

  function click(href: string, init: MouseEventInit = {}, target?: string) {
    const a = document.createElement('a');
    a.href = href;
    if (target) a.target = target;
    const inner = document.createElement('code');
    a.appendChild(inner);
    document.body.appendChild(a);
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init });
    inner.dispatchEvent(ev);
    return ev;
  }

  it('opens another origin in a new tab without an opener', () => {
    const ev = click('https://example.com/docs');
    expect(ev.defaultPrevented).toBe(true);
    expect(open).toHaveBeenCalledWith('https://example.com/docs', '_blank', 'noopener,noreferrer');
  });

  it('leaves same-origin links, modifier clicks, explicit targets and other schemes to the browser', () => {
    expect(click('/sessions/1').defaultPrevented).toBe(false);
    expect(click('https://example.com/', { metaKey: true }).defaultPrevented).toBe(false);
    expect(click('https://example.com/', {}, '_blank').defaultPrevented).toBe(false);
    expect(click('mailto:someone@example.com').defaultPrevented).toBe(false);
    expect(open).not.toHaveBeenCalled();
  });

  it('is gone after uninstall', () => {
    uninstall();
    expect(click('https://example.com/').defaultPrevented).toBe(false);
    uninstall = () => undefined;
  });
});
