import { ActionList, ActionMenu } from '@primer/react';
import { GearIcon, ShieldLockIcon, SignOutIcon } from '@primer/octicons-react';
import { UserAvatar, displayName } from '@/components/UserAvatar';
import { logout, type AuthUser } from '@/lib/api';

interface UserMenuProps {
  user: AuthUser | null;
  onSettingsOpen: () => void;
  onAdminOpen: () => void;
  // Avatar only (the narrow header); the sidebar footer shows the name too.
  compact?: boolean;
}

// UserMenu is the signed-in person's corner: their picture and name open
// Settings, Admin (admins only) and Sign out. Until /auth/me answers the
// trigger is a placeholder so the footer does not jump.
export function UserMenu({ user, onSettingsOpen, onAdminOpen, compact }: UserMenuProps) {
  return (
    <ActionMenu>
      <ActionMenu.Anchor>
        <button type="button" className={'user-menu-trigger' + (compact ? ' user-menu-compact' : '')} aria-label="Account menu" disabled={!user}>
          {user ? <UserAvatar user={user} size={24} /> : <span className="user-avatar" style={{ width: 24, height: 24 }} />}
          {!compact && user && <span className="user-menu-name">{displayName(user)}</span>}
        </button>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay width="small" align={compact ? 'end' : 'start'}>
        <ActionList>
          {user && (
            <ActionList.Item disabled>
              {displayName(user)}
              <ActionList.Description variant="block">{user.email}</ActionList.Description>
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
