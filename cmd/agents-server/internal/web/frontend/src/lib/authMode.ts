import { authConfig, type AuthConfig } from '@/lib/api';
import { useApi } from '@/lib/hooks';

// useAuthMode is the server's auth mode ('token' or 'oauth'), '' until known.
// Token mode is one person: the team controls (Members, Mine | All) hide.
export function useAuthMode(): string {
  const { data } = useApi<AuthConfig>(authConfig, [], 'auth:config');
  return data?.mode || '';
}
