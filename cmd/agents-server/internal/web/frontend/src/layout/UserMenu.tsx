import { useState } from 'react';
import { ActionList, ActionMenu } from '@primer/react';
import { BellIcon, BellSlashIcon, GearIcon, SignOutIcon, SyncIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { logout } from '@/lib/api';
import { loadNotifyPref, notifyUnavailable, saveNotifyPref } from '@/lib/attention';
import { useMe } from '@/lib/me';
import { toast } from '@/lib/toast';

interface UserMenuProps {
  onSettingsOpen: () => void;
  // Avatar only (the narrow header and the rail); the sidebar footer shows the name too.
  compact?: boolean;
  // The trigger's edge the menu lines up with: 'end' for one at the right of the screen.
  align?: 'start' | 'end';
}

// UserMenu is the signed-in person's corner: picture and name open Settings
// (invariant 61), the notification preference and Sign out. Until /auth/me
// answers the trigger is a placeholder so the footer does not jump; after any
// answer the menu opens.
export function UserMenu({ onSettingsOpen, compact, align = 'start' }: UserMenuProps) {
  const { me: user, loading, error, reload } = useMe();
  // Desktop notifications are this browser's preference, asked for here: the
  // browser's own permission prompt opens only on the person's click.
  const [notify, setNotify] = useState(loadNotifyPref);
  const notifyBlocked = notifyUnavailable();
  const toggleNotify = async () => {
    if (notify) {
      saveNotifyPref(false);
      setNotify(false);
      return;
    }
    const granted = Notification.permission === 'granted' || await Notification.requestPermission() === 'granted';
    if (!granted) {
      toast.info('Notifications are blocked for this site — allow them in the browser to turn this on');
      return;
    }
    saveNotifyPref(true);
    setNotify(true);
  };
  return (
    <ActionMenu>
      <ActionMenu.Anchor>
        <button type="button" className={'user-menu-trigger' + (compact ? ' user-menu-compact' : '')} aria-label="Account menu" disabled={loading}>
          {user ? <UserAvatar user={user} size={24} /> : <span className="user-avatar" style={{ width: 24, height: 24 }} />}
          {!compact && user && <span className="user-menu-name">{displayName(user)}</span>}
        </button>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="small" align={align}>
        <ActionList>
          {/* Who this is: a heading, not a menu item, so it is never announced
              as a disabled choice. */}
          {user && (
            <ActionList.Group>
              <ActionList.GroupHeading auxiliaryText={user.email}>{displayName(user)}</ActionList.GroupHeading>
            </ActionList.Group>
          )}
          {error && (
            <ActionList.Item onSelect={reload}>
              <ActionList.LeadingVisual><SyncIcon /></ActionList.LeadingVisual>
              Retry loading account
              <ActionList.Description variant="block">Couldn&apos;t load who is signed in.</ActionList.Description>
            </ActionList.Item>
          )}
          <ActionList.Divider />
          <ActionList.Item onSelect={onSettingsOpen}>
            <ActionList.LeadingVisual><GearIcon /></ActionList.LeadingVisual>
            Settings
          </ActionList.Item>
          <ActionList.Item disabled={!!notifyBlocked} onSelect={() => { void toggleNotify(); }}>
            <ActionList.LeadingVisual>{notify ? <BellIcon /> : <BellSlashIcon />}</ActionList.LeadingVisual>
            {notify ? 'Notifications on' : 'Desktop notifications'}
            {notifyBlocked && <ActionList.Description variant="block">{notifyBlocked}</ActionList.Description>}
          </ActionList.Item>
          <ActionList.Divider />
          <ActionList.Item onSelect={() => { void logout(); }}>
            <ActionList.LeadingVisual><SignOutIcon /></ActionList.LeadingVisual>
            Sign out
          </ActionList.Item>
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  );
}
