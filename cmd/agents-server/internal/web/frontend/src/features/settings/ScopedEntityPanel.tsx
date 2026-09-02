import { lazy, Suspense, useMemo, useState, type ComponentType, type LazyExoticComponent } from 'react';
import { Loading } from '@/components/Loading';
import { ScopeFilterContext } from '@/components/ScopeFilter';
import { useIsAdmin } from '@/lib/me';
import type { ScopedEntity } from '@/features/admin/ScopedRowsPanel';
import './settings.css';

// The panel of each scoped entity, loaded on first show.
const PANEL: Record<Exclude<ScopedEntity, 'workflows'>, LazyExoticComponent<ComponentType>> = {
  providers: lazy(() => import('@/features/providers/ProviderPanel')),
  agents: lazy(() => import('@/features/agents/AgentConfigPanel')),
  'mcp-servers': lazy(() => import('@/features/mcp/McpServerPanel')),
  skills: lazy(() => import('@/features/skills/SkillsPanel')),
};

// ScopedEntityPanel is one scoped entity's settings tab: ONE list, in which
// an admin also sees every other member's rows and gets the "Mine | All"
// filter to narrow it (invariant 61).
export function ScopedEntityPanel({ entity }: { entity: Exclude<ScopedEntity, 'workflows'> }) {
  const isAdmin = useIsAdmin();
  const [mine, setMine] = useState(false);
  const Panel = PANEL[entity];
  const filter = useMemo(() => (isAdmin ? { mine, setMine } : null), [isAdmin, mine]);
  return (
    <ScopeFilterContext value={filter}>
      <Suspense fallback={<Loading kind="panel" />}><Panel /></Suspense>
    </ScopeFilterContext>
  );
}

export const ProvidersTab = () => <ScopedEntityPanel entity="providers" />;
export const AgentsTab = () => <ScopedEntityPanel entity="agents" />;
export const McpServersTab = () => <ScopedEntityPanel entity="mcp-servers" />;
export const SkillsTab = () => <ScopedEntityPanel entity="skills" />;
