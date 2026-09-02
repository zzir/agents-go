import { lazy, Suspense, useState, type ComponentType, type LazyExoticComponent } from 'react';
import { SegmentedControl } from '@primer/react';
import { Loading } from '@/components/Loading';
import { useIsAdmin } from '@/lib/me';
import { ScopedRowsPanel, type ScopedEntity } from '@/features/admin/ScopedRowsPanel';
import './settings.css';

// The personal panel of each scoped entity, loaded on first show.
const PERSONAL: Record<Exclude<ScopedEntity, 'workflows'>, LazyExoticComponent<ComponentType>> = {
  providers: lazy(() => import('@/features/providers/ProviderPanel')),
  agents: lazy(() => import('@/features/agents/AgentConfigPanel')),
  'mcp-servers': lazy(() => import('@/features/mcp/McpServerPanel')),
  skills: lazy(() => import('@/features/skills/SkillsPanel')),
};

// ScopedEntityPanel is one scoped entity's settings tab: the personal panel,
// and for an admin a "Mine | All members" switch to the management view of
// the same rows (invariant 61). Both views stay mounted once shown and
// switch by visibility (invariant 51).
export function ScopedEntityPanel({ entity }: { entity: Exclude<ScopedEntity, 'workflows'> }) {
  const isAdmin = useIsAdmin();
  const [all, setAll] = useState(false);
  const [allVisited, setAllVisited] = useState(false);
  const Personal = PERSONAL[entity];
  const showAll = !!isAdmin && all;
  return (
    <>
      {isAdmin && (
        <div className="scope-toggle">
          <SegmentedControl aria-label="Whose rows" size="small"
            onChange={i => { const next = i === 1; setAll(next); if (next) setAllVisited(true); }}>
            <SegmentedControl.Button selected={!all}>Mine</SegmentedControl.Button>
            <SegmentedControl.Button selected={all}>All members</SegmentedControl.Button>
          </SegmentedControl>
        </div>
      )}
      <div hidden={showAll}>
        <Suspense fallback={<Loading kind="panel" />}><Personal /></Suspense>
      </div>
      {allVisited && (
        <div hidden={!showAll}><ScopedRowsPanel entity={entity} /></div>
      )}
    </>
  );
}

export const ProvidersTab = () => <ScopedEntityPanel entity="providers" />;
export const AgentsTab = () => <ScopedEntityPanel entity="agents" />;
export const McpServersTab = () => <ScopedEntityPanel entity="mcp-servers" />;
export const SkillsTab = () => <ScopedEntityPanel entity="skills" />;
