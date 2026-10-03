// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer's pieces as plain elements: the switch is a checkbox the test clicks.
vi.mock('@primer/react', () => ({
  Button: ({ children, onClick, disabled }: { children?: ReactNode; onClick?: () => void; disabled?: boolean }) => <button type="button" onClick={onClick} disabled={disabled}>{children}</button>,
  TextInput: (p: Record<string, unknown>) => <input {...(p as object)} />,
  Label: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
  Link: ({ children }: { children?: ReactNode }) => <a>{children}</a>,
  Select: Object.assign(
    ({ children, ...p }: { children?: ReactNode }) => <select {...(p as object)}>{children}</select>,
    { Option: ({ children, ...p }: { children?: ReactNode }) => <option {...(p as object)}>{children}</option> },
  ),
  Stack: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  ToggleSwitch: ({ checked, onClick, 'aria-labelledby': by }: { checked?: boolean; onClick?: () => void; 'aria-labelledby'?: string }) => <input type="checkbox" checked={!!checked} onChange={() => {}} onClick={onClick} aria-labelledby={by} />,
  useConfirm: () => async () => false,
}));
vi.mock('@primer/octicons-react', () => new Proxy({}, { get: () => () => null }));
// The field wrappers (lib/form) render as plain boxes.
vi.mock('@/lib/form', () => ({
  fc: (_label: string, control: ReactNode, caption?: ReactNode) => <div>{control}{caption}</div>,
  seg: () => null,
}));
vi.mock('@/components/FormActions', () => ({
  FormActions: ({ onSave, onCancel }: { onSave: () => void; onCancel?: () => void }) => <div><button type="button" onClick={onSave}>Save</button>{onCancel && <button type="button" onClick={onCancel}>Cancel</button>}</div>,
}));
vi.mock('@/components/SecretInput', () => ({ SecretInput: (p: Record<string, unknown>) => <input {...(p as object)} /> }));
vi.mock('@/components/TokenListInput', () => ({ TokenListInput: () => null }));
vi.mock('@/components/CrudPanel', () => ({ CrudPanel: () => null, RowActionsMenu: () => null, ScopeBadge: () => null }));
vi.mock('@/components/ScopeFilter', () => ({ useScopeFilter: () => null }));
vi.mock('@/components/TransferDialog', () => ({ useTransfer: () => ({ dialog: null, start: () => {} }) }));
vi.mock('@/components/ResourceRow', () => ({ ResourceRow: () => null }));
vi.mock('@/lib/JsonField', () => ({ JsonField: () => null }));
vi.mock('@/lib/hooks', () => ({ useCrud: () => ({ items: [] }) }));
vi.mock('@/lib/toast', () => ({ toast: { error: () => {} } }));
const { toolsOf } = vi.hoisted(() => ({ toolsOf: { value: [] as Array<{ name: string; original_name: string; read_only_hint?: boolean; description?: string }> } }));
vi.mock('@/lib/api', () => ({ api: { mcpServers: { tools: async () => toolsOf.value } } }));
import { McpForm, flatten, pack, readOnlyHinted } from '@/features/mcp/McpServerPanel';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

describe('plan-mode allowance of an MCP server', () => {
  it('round-trips read_only_tools through the form and keeps the key out when empty', () => {
    const form = flatten({ name: 's', config: { endpoint: 'http://x', read_only_tools: ['search', 'fetch'] } });
    expect(form.read_only_tools).toEqual(['search', 'fetch']);
    expect(pack(form).config?.read_only_tools).toEqual(['search', 'fetch']);
    expect(pack({ ...form, read_only_tools: [] }).config).not.toHaveProperty('read_only_tools');
  });

  it("adopts the server's read-only hints only on the explicit button", () => {
    expect(readOnlyHinted([
      { name: 'd__search', original_name: 'search', read_only_hint: true },
      { name: 'd__write', original_name: 'write' },
      { name: 'd__fetch', original_name: 'fetch', read_only_hint: true },
    ])).toEqual(['search', 'fetch']);
  });

  // A connected server's tools are listed as switches; a switch names the
  // tool in the save, and the button takes the server's hints wholesale.
  it('lists a connected server\'s tools, saves the picked ones', async () => {
    toolsOf.value = [
      { name: 'd__search', original_name: 'search', read_only_hint: true, description: 'find' },
      { name: 'd__write', original_name: 'write' },
    ];
    const onSave = vi.fn();
    const host = document.createElement('div');
    document.body.appendChild(host);
    const root = createRoot(host);
    await act(async () => {
      root.render(<McpForm initial={{ id: 'm1', name: 'docs', enabled: true, status: 'connected', config: { endpoint: 'http://x' } }} onSave={onSave} />);
    });
    await act(async () => {});
    const labels = [...host.querySelectorAll('.toggle-row-title')].map(e => e.textContent);
    expect(labels).toContain('search');
    expect(labels).toContain('write');
    const switchFor = (name: string) => {
      const title = [...host.querySelectorAll('.toggle-row-title')].find(e => e.textContent === name)!;
      return title.closest('.toggle-row')!.querySelector('input[type=checkbox]') as HTMLInputElement;
    };
    await act(async () => { switchFor('write').click(); });
    await act(async () => { [...host.querySelectorAll('button')].find(b => b.textContent === 'Save')!.click(); });
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onSave.mock.calls[0][0].config.read_only_tools).toEqual(['write']);
    // The hints, adopted on purpose.
    await act(async () => { [...host.querySelectorAll('button')].find(b => b.textContent?.startsWith('Select the ones'))!.click(); });
    await act(async () => { [...host.querySelectorAll('button')].find(b => b.textContent === 'Save')!.click(); });
    expect(onSave.mock.calls[1][0].config.read_only_tools).toEqual(['search']);
    act(() => root.unmount());
    host.remove();
  });
});
