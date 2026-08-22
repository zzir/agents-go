import { ActionList, ActionMenu } from '@primer/react';
import { GearIcon, ShieldLockIcon, SignOutIcon, SyncIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { logout } from '@/lib/api';
import { useMe } from '@/lib/me';

interface UserMenuProps {
  onSettingsOpen: () => void;
  onAdminOpen: () => void;
  // Avatar only (the narrow header); the sidebar footer shows the name too.
  compact?: boolean;
}

// UserMenu is the signed-in person's corner: their picture and name open
// Settings, Admin (admins only) and Sign out. Until /auth/me answers the
// trigger is a placeholder so the footer does not jump; once it has answered
// — even with a failure — the menu opens, so Sign out is always reachable.
export function UserMenu({ onSettingsOpen, onAdminOpen, compact }: UserMenuProps) {
  const { me: user, loading, error, reload } = useMe();
  return (
    <ActionMenu>
      <ActionMenu.Anchor>
        <button type="button" className={'user-menu-trigger' + (compact ? ' user-menu-compact' : '')} aria-label="Account menu" disabled={loading}>
          {user ? <UserAvatar user={user} size={24} /> : <span className="user-avatar" style={{ width: 24, height: 24 }} />}
          {!compact && user && <span className="user-menu-name">{displayName(user)}</span>}
        </button>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="small" align={compact ? 'end' : 'start'}>
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
          {user?.role === 'admin' && (
            <ActionList.Item onSelect={onAdminOpen}>
              <ActionList.LeadingVisual><ShieldLockIcon /></ActionList.LeadingVisual>
              Admin
            </ActionList.Item>
          )}
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
