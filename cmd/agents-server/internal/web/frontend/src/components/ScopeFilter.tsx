import { createContext, useContext } from 'react';
import { SegmentedControl } from '@primer/react';

// ScopeFilter narrows a scoped entity's list to the caller's own rows. The
// state is set by an admin's settings tab (invariant 61) — the one list that
// holds other members' rows — and absent elsewhere, so the control renders
// nothing on a member's panel.
export interface ScopeFilterState {
  mine: boolean;
  setMine: (mine: boolean) => void;
}

export const ScopeFilterContext = createContext<ScopeFilterState | null>(null);

export function useScopeFilter(): ScopeFilterState | null {
  return useContext(ScopeFilterContext);
}

export function ScopeFilter() {
  const f = useScopeFilter();
  if (!f) return null;
  return (
    <SegmentedControl aria-label="Whose rows" size="small" onChange={i => f.setMine(i === 0)}>
      <SegmentedControl.Button selected={f.mine}>Mine</SegmentedControl.Button>
      <SegmentedControl.Button selected={!f.mine}>All</SegmentedControl.Button>
    </SegmentedControl>
  );
}
