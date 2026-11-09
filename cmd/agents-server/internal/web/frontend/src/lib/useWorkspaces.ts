import { useEffect } from 'react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import type { Workspace } from '@/lib/binding';

/* useWorkspaces fetches the caller's workspace rows — the composer's Workspace
   submenu and the terminal panel's + menu both read them. One hook, two
   consumers.

   version is the app's counter of workspace-set changes (a first run bound a
   session and may have auto-created its scratch workspace, a session was
   deleted); a bump refetches so a picker opened later offers the new workspace. */
export function useWorkspaces(
  version: number | undefined,
): {
  workspaces: Workspace[] | null;
  error: string | null;
  reload: () => void;
  mutate: (fn: (prev: Workspace[] | null) => Workspace[] | null) => void;
} {
  const { data, error, reload, mutateData } = useApi<Workspace[]>(() => api.workspaces.list() as Promise<Workspace[]>, [], 'workspaces');
  useEffect(() => {
    if (version) reload();
  }, [version, reload]);
  return { workspaces: data, error, reload, mutate: mutateData };
}
