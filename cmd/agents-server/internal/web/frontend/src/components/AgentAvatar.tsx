import { useState } from 'react';

// AgentAvatar: the agent's built-in picture when set (and it loads), otherwise
// the first letter of its name — the same circle UserAvatar draws for people.
export function AgentAvatar({ name, avatar, size = 20 }: { name?: string; avatar?: string; size?: number }) {
  const [failed, setFailed] = useState<string | null>(null);
  const style = { width: size, height: size, fontSize: Math.round(size * 0.45) };
  return avatar && failed !== avatar
    ? <img className="user-avatar" style={style} src={avatar} alt="" onError={() => setFailed(avatar)} />
    : <span className="user-avatar" style={style} aria-hidden>{(name || '').trim() ? name!.trim()[0].toUpperCase() : '?'}</span>;
}
