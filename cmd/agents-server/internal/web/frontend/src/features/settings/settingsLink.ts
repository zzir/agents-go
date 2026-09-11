// openSettingsTab switches the open Settings dialog to another tab through the
// hash deep link the app consumes (lib/route.ts): a panel has no handle on the
// dialog's own tab state.
export function openSettingsTab(tab: string): void {
  window.location.hash = '#/settings/' + tab;
}
