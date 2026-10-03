import { currentViewHash, settingsHash } from '@/lib/route';

// openSettingsTab switches the open Settings dialog to another tab through the
// URL the app reads (lib/route.ts): a panel has no handle on the dialog's own
// tab state. A tab switch replaces the entry, so Back still closes the dialog.
export function openSettingsTab(tab: string): void {
  window.history.replaceState(null, '', settingsHash(currentViewHash(), tab));
  window.dispatchEvent(new HashChangeEvent('hashchange'));
}
