// @vitest-environment jsdom
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the confirm answers what
// the test set.
const primer = vi.hoisted(() => ({ answer: true, confirm: vi.fn(async () => primer.answer) }));
vi.mock('@primer/react', () => ({
  Button: (p: Record<string, unknown>) => <button type="button" onClick={p.onClick as () => void}>{p.children as string}</button>,
  useConfirm: () => primer.confirm,
}));
import { FormDirtyContext } from '@/lib/unsaved';
import { FormActions } from './FormActions';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { primer.confirm.mockClear(); primer.answer = true; });

async function clickCancel(dirty: boolean) {
  const onCancel = vi.fn();
  const el = document.createElement('div');
  const root = createRoot(el);
  await act(async () => root.render(
    <FormDirtyContext value={dirty}><FormActions onSave={() => undefined} onCancel={onCancel} /></FormDirtyContext>,
  ));
  const cancel = [...el.querySelectorAll('button')].find(b => b.textContent === 'Cancel')!;
  await act(async () => { cancel.click(); });
  await act(async () => root.unmount());
  return onCancel;
}

describe('FormActions Cancel', () => {
  it('asks before discarding an edited form, and keeps it on no', async () => {
    primer.answer = false;
    const onCancel = await clickCancel(true);
    expect(primer.confirm).toHaveBeenCalledTimes(1);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('discards on yes', async () => {
    const onCancel = await clickCancel(true);
    expect(primer.confirm).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('closes a pristine form without asking', async () => {
    const onCancel = await clickCancel(false);
    expect(primer.confirm).not.toHaveBeenCalled();
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
