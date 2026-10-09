import { currentViewHash, settingsHash } from '@/lib/route';

// openSettingsTab switches the open Settings dialog to another tab through the
// URL (lib/route.ts); replaceState, so Back still closes the dialog.
export function openSettingsTab(tab: string): void {
  window.history.replaceState(null, '', settingsHash(currentViewHash(), tab));
  window.dispatchEvent(new HashChangeEvent('hashchange'));
}
