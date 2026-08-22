import { useState } from 'react';

// The fields a picture and a name need; both AuthUser and a store.User row
// carry them.
export interface Person { name?: string; email?: string; avatar_url?: string }

// UserAvatar: the provider's picture when there is one (and it loads),
// otherwise the first letter of the name. The picture is cross-origin (CSP
// admits the provider's hosts); no referrer leaks the workbench URL to it.
export function UserAvatar({ user, size = 32 }: { user: Person; size?: number }) {
  const [failed, setFailed] = useState<string | null>(null);
  const style = { width: size, height: size, fontSize: Math.round(size * 0.45) };
  return user.avatar_url && failed !== user.avatar_url
    ? <img className="user-avatar" style={style} src={user.avatar_url} alt="" referrerPolicy="no-referrer" onError={() => setFailed(user.avatar_url || null)} />
    : <span className="user-avatar" style={style} aria-hidden>{initialOf(user)}</span>;
}

export function displayName(u: Person): string {
  return u.name || u.email || '';
}

function initialOf(u: Person): string {
  const s = displayName(u).trim();
  return s ? s[0].toUpperCase() : '?';
}
