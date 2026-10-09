import './terminal.css';
import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { ActionMenu, ActionList, IconButton } from '@primer/react';
import { ChevronDownIcon, PlusIcon, QuoteIcon, SyncIcon, TerminalIcon, XIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { readStoredSize, saveStoredSize, useApi } from '@/lib/hooks';
import { insertIntoComposer, quoteAsCodeBlock } from '@/lib/composer';
import { useProjects } from '@/lib/useProjects';
import { toast } from '@/lib/toast';
import { TerminalView, type TerminalViewHandle, type TermStatus } from '@/features/terminal/TerminalView';

interface SandboxTarget {
  id: string;
  name: string;
}

interface TerminalTab {
  id: number;
  // The project whose container the shell opened into — a bound session's
  // request lands the terminal in its own project container.
  projectId: string;
  projectName: string;
  // The machine the project lives on, for the tab label.
  targetName: string;
  // gen forces a fresh session (remount) on restart.
  gen: number;
  status: TermStatus;
}

interface TerminalPanelProps {
  open: boolean;
  onClose: () => void;
  settingsReloadKey?: number;
  // Bumped by the app when the set of session bindings changed; refreshes the
  // + menu's project list.
  bindingsVersion?: number;
  // One-shot request to start (or focus) a terminal for a project, issued when
  // the top-bar button opens a closed panel; the nonce marks each request as new.
  openRequest?: { projectId: string; projectName?: string; targetName?: string; nonce: number } | null;
}

// Dragging the top edge below this height collapses the panel to just its
// header bar (sessions keep running); dragging back up past it re-expands.
const COLLAPSE_AT = 80;
const MIN_HEIGHT = 120;
const DEFAULT_HEIGHT = 300;
const HEIGHT_KEY = 'terminalHeight';
const HEIGHT_ARROW_KEY_STEP = 10;
const maxHeight = () => Math.round(window.innerHeight * 0.8);

// TerminalPanel is the global bottom panel hosting sandbox terminals in tabs;
// it stays mounted while hidden, and only closing a tab ends its shell (invariant 81).
export function TerminalPanel({ open, onClose, settingsReloadKey, bindingsVersion, openRequest }: TerminalPanelProps) {
  const [tabs, setTabs] = useState<TerminalTab[]>([]);
  const [activeId, setActiveId] = useState<number | null>(null);
  const nextId = useRef(1);
  const dragRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(() => readStoredSize(HEIGHT_KEY, DEFAULT_HEIGHT));
  // The drag handlers read it through the ref, set eagerly so a burst of
  // moves between renders sees its own changes.
  const heightRef = useRef(height);
  heightRef.current = height;
  const [dragging, setDragging] = useState(false);
  // Collapsed = header-only strip, aligned with the left sidebar's footer.
  const [collapsed, setCollapsed] = useState(false);
  // Selection state of the ACTIVE tab, driving the quote button; the handles
  // let the quote action pull the selection text on demand.
  const [activeHasSelection, setActiveHasSelection] = useState(false);
  const viewRefs = useRef(new Map<number, TerminalViewHandle | null>());

  const { data: targets, reload: reloadTargets } = useApi<SandboxTarget[]>(
    () => api.sandboxes.list() as Promise<SandboxTarget[]>, [], 'sandboxes',
  );
  useEffect(() => {
    if (settingsReloadKey) reloadTargets();
  }, [settingsReloadKey, reloadTargets]);
  // The caller's project rows for the + menu, the same hook the composer
  // picker uses: a project's terminal lands in that project's container.
  const { projects, error: projectsError } = useProjects(bindingsVersion);

  // A collapsed panel must expand before a terminal can be shown (a new tab
  // mounted into a zero-height body would fit to a bogus grid).
  const expand = () => setCollapsed(false);

  const addTab = (targetName: string, project: { id: string; name: string }) => {
    const id = nextId.current++;
    setTabs(t => [...t, { id, projectId: project.id, projectName: project.name, targetName, gen: 0, status: 'connecting' }]);
    setActiveId(id);
    expand();
  };

  const closeTab = (id: number) => {
    const idx = tabs.findIndex(tab => tab.id === id);
    const next = tabs.filter(tab => tab.id !== id);
    setTabs(next);
    if (activeId === id) {
      setActiveId(next.length ? next[Math.min(idx, next.length - 1)].id : null);
    }
    // Closing the last tab dismisses the whole panel — an empty strip left
    // open is just dead space; the composer button brings it back.
    if (next.length === 0) {
      onClose();
    }
  };

  const restartActive = () => {
    setTabs(t => t.map(tab => (tab.id === activeId ? { ...tab, gen: tab.gen + 1, status: 'connecting' } : tab)));
  };

  const activateTab = (id: number) => {
    setActiveId(id);
    setActiveHasSelection(!!viewRefs.current.get(id)?.getSelection());
    expand();
  };

  // Roving tabindex: the active tab is the list's one Tab stop, arrow keys move
  // along the strip. Only a tab's own keys count (the "+" menu uses Home/End).
  const tabRefs = useRef(new Map<number, HTMLDivElement | null>());
  const onTabListKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (tabs.length === 0 || (e.target as HTMLElement).getAttribute('role') !== 'tab') return;
    const idx = tabs.findIndex(t => t.id === activeId);
    let next: number;
    switch (e.key) {
      case 'ArrowLeft': next = idx <= 0 ? tabs.length - 1 : idx - 1; break;
      case 'ArrowRight': next = idx < 0 || idx >= tabs.length - 1 ? 0 : idx + 1; break;
      case 'Home': next = 0; break;
      case 'End': next = tabs.length - 1; break;
      default: return;
    }
    e.preventDefault();
    const id = tabs[next].id;
    activateTab(id);
    tabRefs.current.get(id)?.focus();
  };

  // Quote the active tab's selection into the chat composer as a code block.
  const quoteSelection = () => {
    const sel = activeId !== null ? viewRefs.current.get(activeId)?.getSelection() : '';
    if (!sel || !sel.trim()) {
      // hasSelection can be true for pure-whitespace cells; say so instead of
      // silently doing nothing.
      toast.info('Select some terminal output first');
      return;
    }
    if (!insertIntoComposer(quoteAsCodeBlock(sel))) {
      toast.warn('Open a session to quote into');
    }
  };

  const setTabStatus = (id: number, status: TermStatus) => {
    setTabs(t => t.map(tab => (tab.id === id ? { ...tab, status } : tab)));
  };

  // Consume the composer's one-shot open request: focus the most recent tab
  // already running on that project, or start a fresh terminal for it.
  const consumedRequestNonce = useRef(0);
  useEffect(() => {
    if (!openRequest || openRequest.nonce === consumedRequestNonce.current) return;
    consumedRequestNonce.current = openRequest.nonce;
    const matching = tabs.filter(t => t.projectId === openRequest.projectId);
    const existing = matching[matching.length - 1];
    if (existing) {
      activateTab(existing.id);
    } else {
      // The requester (ChatView) knows the project's name even before this
      // panel's projects fetch lands, so a tab never opens nameless.
      const project = (projects || []).find(p => p.id === openRequest.projectId);
      const name = project?.name || openRequest.projectName || '';
      const targetName = (targets || []).find(t => t.id === project?.sandbox_id)?.name || openRequest.targetName || '';
      addTab(targetName, { id: openRequest.projectId, name });
    }
    // activateTab/addTab close over current state; nonce guards re-runs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openRequest]);

  // Drag-to-resize on the panel's top edge: pointer events, so a touch drag
  // resizes too, with capture holding the drag past the strip's few pixels.
  useEffect(() => {
    const handle = dragRef.current;
    if (!handle) return;
    let startY = 0;
    let startHeight = 0;
    const onMove = (move: PointerEvent) => {
      if (!handle.hasPointerCapture(move.pointerId)) return;
      const raw = startHeight + (startY - move.clientY);
      if (raw < COLLAPSE_AT) {
        // Dragged (nearly) to the bottom: collapse to the header strip.
        setCollapsed(true);
        return;
      }
      setCollapsed(false);
      const next = Math.min(Math.max(raw, MIN_HEIGHT), maxHeight());
      heightRef.current = next;
      setHeight(next);
    };
    const onUp = () => {
      setDragging(false);
      saveStoredSize(HEIGHT_KEY, heightRef.current);
    };
    const onDown = (down: PointerEvent) => {
      if (down.button !== 0) return;
      down.preventDefault();
      try { handle.setPointerCapture(down.pointerId); } catch { /* capture is a nice-to-have */ }
      startY = down.clientY;
      startHeight = panelRef.current?.offsetHeight ?? 300;
      setDragging(true);
    };
    handle.addEventListener('pointerdown', onDown);
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onUp);
    handle.addEventListener('lostpointercapture', onUp);
    return () => {
      handle.removeEventListener('pointerdown', onDown);
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onUp);
      handle.removeEventListener('lostpointercapture', onUp);
    };
  }, []);

  // The keyboard's resize: ArrowUp grows, ArrowDown shrinks and, at the
  // minimum, collapses; ArrowUp on a collapsed panel expands it.
  const onResizeKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
    e.preventDefault();
    const step = e.key === 'ArrowUp' ? HEIGHT_ARROW_KEY_STEP : -HEIGHT_ARROW_KEY_STEP;
    if (collapsed) {
      if (step > 0) setCollapsed(false);
      return;
    }
    if (step < 0 && heightRef.current <= MIN_HEIGHT) {
      setCollapsed(true);
      return;
    }
    const next = Math.min(Math.max(heightRef.current + step, MIN_HEIGHT), maxHeight());
    if (next === heightRef.current) return;
    heightRef.current = next;
    setHeight(next);
    saveStoredSize(HEIGHT_KEY, next);
  };

  return (
    <div
      ref={panelRef}
      className={'terminal-panel' + (collapsed ? ' terminal-panel-collapsed' : '')}
      style={{ height: collapsed ? undefined : height, display: open ? undefined : 'none' }}
    >
      <div
        ref={dragRef}
        className={'terminal-panel-resize pane-resize-handle' + (dragging ? ' dragging' : '')}
        role="slider"
        aria-orientation="vertical"
        aria-label="Resize terminal panel"
        aria-valuemin={0}
        aria-valuemax={maxHeight()}
        aria-valuenow={collapsed ? 0 : height}
        aria-valuetext={collapsed ? 'Terminal panel collapsed to its header' : `Terminal panel height ${height} pixels`}
        tabIndex={0}
        onKeyDown={onResizeKeyDown}
      />
      <div className="terminal-panel-header">
        <div className="terminal-panel-tabs" role="tablist" onKeyDown={onTabListKeyDown}>
          {tabs.map((tab, i) => (
            <div
              key={tab.id}
              ref={el => { if (el) tabRefs.current.set(tab.id, el); else tabRefs.current.delete(tab.id); }}
              role="tab"
              aria-selected={tab.id === activeId}
              tabIndex={tab.id === activeId || (activeId === null && i === 0) ? 0 : -1}
              className={'terminal-tab' + (tab.id === activeId ? ' terminal-tab-active' : '')}
              onClick={() => activateTab(tab.id)}
              onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activateTab(tab.id); } }}
            >
              <TerminalIcon size={12} />
              <span className="terminal-tab-name" title={tab.projectName || undefined}>
                {tab.targetName ? `${tab.targetName} · ${tab.projectName}` : tab.projectName}
              </span>
              {(tab.status === 'exited' || tab.status === 'error') && (
                <span className="terminal-tab-status">{tab.status === 'exited' ? 'exited' : 'lost'}</span>
              )}
              <IconButton
                icon={XIcon}
                variant="invisible"
                size="small"
                className="terminal-tab-close"
                aria-label={`Close ${tab.projectName} terminal`}
                onClick={e => { e.stopPropagation(); closeTab(tab.id); }}
              />
            </div>
          ))}
          <ActionMenu>
            <ActionMenu.Anchor>
              <IconButton icon={PlusIcon} variant="invisible" size="small" aria-label="New" />
            </ActionMenu.Anchor>
            <ActionMenu.Overlay>
              <ActionList>
                {(targets || []).length === 0 ? (
                  <ActionList.Item disabled>No sandbox targets configured</ActionList.Item>
                ) : (
                  (targets || []).map(tg => {
                    const items = (projects || []).filter(p => p.sandbox_id === tg.id);
                    return (
                      <ActionList.Group key={tg.id}>
                        {/* Menu-role ActionList: the heading is presentational,
                            a heading level (`as`) is invalid here and throws. */}
                        <ActionList.GroupHeading>{tg.name}</ActionList.GroupHeading>
                        {items.length === 0 ? (
                          // A failed fetch must not read as an empty account.
                          <ActionList.Item disabled>{projectsError ? 'projects failed to load' : 'no projects yet'}</ActionList.Item>
                        ) : (
                          items.map(p => (
                            <ActionList.Item
                              key={p.id}
                              onSelect={() => addTab(tg.name, { id: p.id, name: p.name })}
                            >
                              {p.name}
                            </ActionList.Item>
                          ))
                        )}
                      </ActionList.Group>
                    );
                  })
                )}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        </div>
        <div className="terminal-panel-actions">
          {activeId !== null && (
            <>
              <IconButton
                icon={QuoteIcon}
                variant="invisible"
                size="small"
                aria-label="Quote selection to the composer"
                disabled={!activeHasSelection}
                // Keep focus (and the xterm selection) where they are.
                onMouseDown={e => e.preventDefault()}
                onClick={quoteSelection}
              />
              <IconButton
                icon={SyncIcon}
                variant="invisible"
                size="small"
                aria-label="Restart terminal"
                onClick={restartActive}
              />
            </>
          )}
          {/* Hides the panel; the shells keep running (invariant 81). */}
          <IconButton icon={ChevronDownIcon} variant="invisible" size="small" aria-label="Hide terminal panel" onClick={onClose} />
        </div>
      </div>
      {tabs.length === 0 ? (
        <div className="terminal-panel-empty">
          <TerminalIcon size={20} />
          <span>Open a terminal with <PlusIcon size={14} /> — sessions keep running while the panel is hidden.</span>
        </div>
      ) : (
        <div className="terminal-panel-body">
          {tabs.map(tab => (
            <TerminalView
              key={`${tab.id}:${tab.gen}`}
              ref={h => {
                if (h) viewRefs.current.set(tab.id, h);
                else viewRefs.current.delete(tab.id);
              }}
              projectId={tab.projectId}
              hidden={tab.id !== activeId}
              onStatus={s => setTabStatus(tab.id, s)}
              onSelection={has => {
                if (tab.id === activeId) setActiveHasSelection(has);
              }}
            />
          ))}
        </div>
      )}
    </div>
  );
}
