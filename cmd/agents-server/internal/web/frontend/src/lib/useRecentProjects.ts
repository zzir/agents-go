import { useEffect } from 'react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { recentProjects, type ProjectOption, type SandboxConfigLite } from '@/lib/binding';

/* useRecentProjects aggregates bound sessions into the "recent projects" the
   composer picker and the terminal panel's + menu both offer. One hook, two
   consumers — each used to fetch its own copy of the sessions list and wire
   its own bindingsVersion effect, verbatim.

   bindingsVersion is the app's counter of binding-set changes (a first run
   bound a session, a session was deleted); a bump refetches so a picker
   opened later offers the new project. */
export function useRecentProjects(
  configs: SandboxConfigLite[] | null,
  bindingsVersion: number | undefined,
): ProjectOption[] {
  const { data: sessionRows, reload } = useApi<Array<{ sandbox_id?: string; work_dir?: string }>>(
    () => api.sessions.list() as Promise<Array<{ sandbox_id?: string; work_dir?: string }>>,
  );
  useEffect(() => {
    if (bindingsVersion) reload();
  }, [bindingsVersion, reload]);
  return recentProjects(sessionRows, configs);
}
