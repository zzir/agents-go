import React, { useState, useEffect, useMemo } from 'react';
import { Dialog, NavList as PrimerNavList, Flash } from '@primer/react';
import { LockIcon } from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { ErrorBoundary } from '@/components/ErrorBoundary';
import { ReadOnlyContext } from '@/lib/access';
import { useNarrow } from '@/lib/hooks';

export interface DialogTab {
  key: string;
  label: string;
  icon: Icon;
  load: () => Promise<{ default: React.ComponentType }>;
  dividerBefore?: boolean;
  // Rows carry their own scope/owner, so a member writes there too — the
  // dialog's blanket read-only applies only to the other tabs.
  scoped?: boolean;
}

function TabLoadError() {
  return <Flash variant="danger">Failed to load this panel — reload the page.</Flash>;
}

// PanelDialog is the one settings hub (invariant 61): a nav of lazily loaded
// panels, the admin group under its own heading, one panel shown at a time.
// readOnly is a member's dialog: shared configuration is theirs to read (the
// API allows it) and not to write (the server refuses with 403), so the
// panels show and offer nothing. readOnly null is "not known yet" (/auth/me
// still loading): the nav shows, the panel waits, so an admin never sees the
// read-only note flash.
export function PanelDialog({ title, tabs, adminTabs, readOnly, onClose }: {
  title: string;
  tabs: DialogTab[];
  adminTabs?: DialogTab[];
  readOnly?: boolean | null;
  onClose: () => void;
}) {
  const [tab, setTab] = useState(tabs[0].key);
  // Keep-alive (invariant 51): a tab's panel is loaded on first visit and
  // then STAYS mounted (hidden). The value is the loaded component, or
  // TabLoadError if its chunk 404'd.
  const [loaded, setLoaded] = useState<Record<string, React.ComponentType>>({});
  const all = useMemo(() => (adminTabs ? [...tabs, ...adminTabs] : tabs), [tabs, adminTabs]);

  const narrow = useNarrow();

  useEffect(() => {
    if (loaded[tab]) return;
    let stale = false;
    const entry = all.find(t => t.key === tab);
    if (!entry) return;
    // Never overwrite a key already loaded: a slow first-load chunk resolving
    // after a faster later click must not clobber the panel that click mounted.
    entry.load().then(mod => {
      if (!stale) setLoaded(prev => (prev[tab] ? prev : { ...prev, [tab]: mod.default }));
    }).catch(() => {
      if (!stale) setLoaded(prev => (prev[tab] ? prev : { ...prev, [tab]: TabLoadError }));
    });
    return () => { stale = true; };
  }, [tab, all, loaded]);

  const item = (t: DialogTab) => (
    <PrimerNavList.Item
      key={t.key}
      aria-current={tab === t.key ? 'page' : undefined}
      onClick={() => setTab(t.key)}
    >
      <PrimerNavList.LeadingVisual><t.icon size={16} /></PrimerNavList.LeadingVisual>
      {t.label}
    </PrimerNavList.Item>
  );

  return (
    <Dialog
      title={title}
      onClose={() => onClose()}
      height="auto"
      position={{ narrow: 'fullscreen', regular: 'center' }}
      // Both sides scale with the viewport and cap, so the dialog stays a
      // landscape box on a large screen instead of a column. The width cap is
      // the nav plus the 1100px content column plus margins.
      style={narrow ? undefined : { width: 'clamp(960px, 80dvw, 1360px)', height: 'clamp(560px, 85dvh, 1000px)' }}
      renderBody={({ children }) => (
        <Dialog.Body className="settings-body" style={{ padding: 0 }}>
          {children}
        </Dialog.Body>
      )}
    >
      <div className="settings-layout">
        <nav className="settings-nav">
          <PrimerNavList aria-label={`${title} sections`}>
            {tabs.map(t => (
              <React.Fragment key={t.key}>
                {t.dividerBefore && tabs[0] !== t && <PrimerNavList.Divider />}
                {item(t)}
              </React.Fragment>
            ))}
            {adminTabs && adminTabs.length > 0 && (
              <>
                <PrimerNavList.Divider />
                {adminTabs.map(item)}
              </>
            )}
          </PrimerNavList>
        </nav>
        <div className="settings-content">
          {readOnly !== null && all.map(t => {
            const Comp = loaded[t.key];
            if (!Comp) return null; // never visited → never mounted
            const showNote = !!readOnly && t.key !== 'account' && !t.scoped;
            return (
              <div key={t.key} className="settings-panel" hidden={t.key !== tab}>
                {showNote && (
                  <Flash variant="default" className="settings-readonly-note">
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
                      <LockIcon size={16} />
                      Read-only. Shared configuration is managed by admins; you can use all of it in your own sessions.
                    </span>
                  </Flash>
                )}
                <ReadOnlyContext value={!!readOnly && !t.scoped}>
                  <ErrorBoundary resetKey={t.key}><Comp /></ErrorBoundary>
                </ReadOnlyContext>
              </div>
            );
          })}
        </div>
      </div>
    </Dialog>
  );
}
