import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';

// The start-up configuration (GET /server): shown, never edited — it comes
// from the command line, not the settings table.
export interface ServerInfo {
  version: string;
  // The IANA zone cron schedules are read in.
  timezone?: string;
  // False when no AGENTS_SECRET_KEY / --secret-key-file seals stored credentials.
  credentials_sealed?: boolean;
}

export const SERVER_INFO_KEY = 'server';

// useServerInfo is the one fetch of /server, shared by every reader.
export function useServerInfo() {
  return useApi<ServerInfo>(() => api.server() as Promise<ServerInfo>, [], SERVER_INFO_KEY);
}

export const UNSEALED_TEXT = 'Credentials at rest: unsealed — set AGENTS_SECRET_KEY or --secret-key-file';
