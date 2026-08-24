import { useEffect } from 'react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import type { Project } from '@/lib/binding';

/* useProjects fetches the caller's project rows — the composer picker and the
   terminal panel's + menu both group them by sandbox. One hook, two consumers.

   version is the app's counter of project-set changes (a first run bound a
   session and may have auto-created its scratch project, a session was
   deleted); a bump refetches so a picker opened later offers the new project. */
export function useProjects(
  version: number | undefined,
): {
  projects: Project[] | null;
  error: string | null;
  reload: () => void;
  mutate: (fn: (prev: Project[] | null) => Project[] | null) => void;
} {
  const { data, error, reload, mutateData } = useApi<Project[]>(() => api.projects.list() as Promise<Project[]>);
  useEffect(() => {
    if (version) reload();
  }, [version, reload]);
  return { projects: data, error, reload, mutate: mutateData };
}
