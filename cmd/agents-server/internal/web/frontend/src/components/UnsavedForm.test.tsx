// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { act, useContext } from 'react';
import { createRoot } from 'react-dom/client';
import { FormDirtyContext, UnsavedContext, type UnsavedRegistry } from '@/lib/unsaved';
import { UnsavedForm } from './UnsavedForm';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

function Flag() { return <output>{String(useContext(FormDirtyContext))}</output>; }

describe('UnsavedForm', () => {
  it('reports an edit to the dialog and the form, and clears on unmount', () => {
    const set = vi.fn();
    const registry: UnsavedRegistry = { set, any: () => false };
    const el = document.createElement('div');
    document.body.appendChild(el);
    const root = createRoot(el);
    act(() => root.render(
      <UnsavedContext value={registry}>
        <UnsavedForm><input /><Flag /></UnsavedForm>
      </UnsavedContext>,
    ));
    const flag = () => el.querySelector('output')!.textContent;
    expect(flag()).toBe('false');
    expect(set).toHaveBeenLastCalledWith(expect.any(String), false);
    act(() => { el.querySelector('input')!.dispatchEvent(new Event('input', { bubbles: true })); });
    expect(flag()).toBe('true');
    expect(set).toHaveBeenLastCalledWith(expect.any(String), true);
    const id = set.mock.lastCall![0];
    act(() => root.unmount());
    expect(set).toHaveBeenLastCalledWith(id, false);
    el.remove();
  });

  it('stands alone outside a dialog', () => {
    const el = document.createElement('div');
    const root = createRoot(el);
    act(() => root.render(<UnsavedForm><input /><Flag /></UnsavedForm>));
    act(() => { el.querySelector('input')!.dispatchEvent(new Event('input', { bubbles: true })); });
    expect(el.querySelector('output')!.textContent).toBe('true');
    act(() => root.unmount());
  });
});
