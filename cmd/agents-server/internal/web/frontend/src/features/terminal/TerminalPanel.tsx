import './terminal.css';
import { useEffect, useRef, useState } from 'react';
import { ActionMenu, ActionList, IconButton } from '@primer/react';
import { PlusIcon, QuoteIcon, SyncIcon, TerminalIcon, XIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { insertIntoComposer, quoteAsCodeBlock } from '@/lib/composer';
import { toast } from '@/lib/toast';
import { TerminalView, type TerminalViewHandle, type TermStatus } from '@/features/terminal/TerminalView';

interface SandboxConfig {
  id: string;
  name: string;
  terminal?: boolean;
}

interface TerminalTab {
  id: number;
  sandboxId: string;
  sandboxName: string;
  // gen forces a fresh session (remount) on restart.
  gen: number;
  status: TermStatus;
}

interface TerminalPanelProps {
  open: boolean;
  onClose: () => void;
  settingsReloadKey?: number;
  // One-shot request to start (or focus) a terminal for a sandbox, issued
  // when the composer button opens a closed panel with a capable sandbox
  // selected. The nonce marks each request as new.
  openRequest?: { id: string; name: string; nonce: number } | null;
}

// Dragging the top edge below this height collapses the panel to just its
// header bar (sessions keep running); dragging back up past it re-expands.
const COLLAPSE_AT = 80;
const MIN_HEIGHT = 120;

// TerminalPanel is the global bottom panel hosting sandbox terminals in tabs.
// It is session-agnostic and stays mounted while hidden so every tab's shell
// survives panel toggles, chat switches and sandbox re-selection; only
// closing a tab (or the page) ends that session.
export function TerminalPanel({ open, onClose, settingsReloadKey, openRequest }: TerminalPanelProps) {
  const [tabs, setTabs] = useState<TerminalTab[]>([]);
  const [activeId, setActiveId] = useState<number | null>(null);
  const nextId = useRef(1);
  const dragRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(300);
  const [dragging, setDragging] = useState(false);
  // Collapsed = header-only strip, aligned with the left sidebar's footer.
  const [collapsed, setCollapsed] = useState(false);
  // Selection state of the ACTIVE tab, driving the quote button; the handles
  // let the quote action pull the selection text on demand.
  const [activeHasSelection, setActiveHasSelection] = useState(false);
  const viewRefs = useRef(new Map<number, TerminalViewHandle | null>());

  const { data: sandboxes, reload: reloadSandboxes } = useApi<SandboxConfig[]>(
    () => api.sandboxes.list() as Promise<SandboxConfig[]>,
  );
  useEffect(() => {
    if (settingsReloadKey) reloadSandboxes();
  }, [settingsReloadKey, reloadSandboxes]);
  const capable = (sandboxes || []).filter(s => s.terminal);

  // A collapsed panel must expand before a terminal can be shown (a new tab
  // mounted into a zero-height body would fit to a bogus grid).
  const expand = () => setCollapsed(false);

  const addTab = (s: SandboxConfig) => {
    const id = nextId.current++;
    setTabs(t => [...t, { id, sandboxId: s.id, sandboxName: s.name, gen: 0, status: 'connecting' }]);
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
      toast.warn('Open a chat to quote into');
    }
  };

  const setTabStatus = (id: number, status: TermStatus) => {
    setTabs(t => t.map(tab => (tab.id === id ? { ...tab, status } : tab)));
  };

  // Consume the composer's one-shot open request: focus the most recent tab
  // already running on that sandbox, or start a fresh terminal for it.
  const consumedRequestNonce = useRef(0);
  useEffect(() => {
    if (!openRequest || openRequest.nonce === consumedRequestNonce.current) return;
    consumedRequestNonce.current = openRequest.nonce;
    const matching = tabs.filter(t => t.sandboxId === openRequest.id);
    const existing = matching[matching.length - 1];
    if (existing) {
      activateTab(existing.id);
    } else {
      addTab({ id: openRequest.id, name: openRequest.name });
    }
    // activateTab/addTab close over current state; nonce guards re-runs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openRequest]);

  // Drag-to-resize on the panel's top edge.
  useEffect(() => {
    const handle = dragRef.current;
    if (!handle) return;
    const onMouseDown = (down: MouseEvent) => {
      down.preventDefault();
      const startY = down.clientY;
      const startHeight = panelRef.current?.offsetHeight ?? 300;
      setDragging(true);
      const onMove = (move: MouseEvent) => {
        const raw = startHeight + (startY - move.clientY);
        if (raw < COLLAPSE_AT) {
          // Dragged (nearly) to the bottom: collapse to the header strip.
          setCollapsed(true);
          return;
        }
        setCollapsed(false);
        setHeight(Math.min(Math.max(raw, MIN_HEIGHT), Math.round(window.innerHeight * 0.8)));
      };
      const onUp = () => {
        setDragging(false);
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
      };
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    };
    handle.addEventListener('mousedown', onMouseDown);
    return () => handle.removeEventListener('mousedown', onMouseDown);
  }, []);

  return (
    <div
      ref={panelRef}
      className={'terminal-panel' + (collapsed ? ' terminal-panel-collapsed' : '')}
      style={{ height: collapsed ? undefined : height, display: open ? undefined : 'none' }}
    >
      <div ref={dragRef} className={'terminal-panel-resize pane-resize-handle' + (dragging ? ' dragging' : '')} />
      <div className="terminal-panel-header">
        <div className="terminal-panel-tabs" role="tablist">
          {tabs.map(tab => (
            <div
              key={tab.id}
              role="tab"
              aria-selected={tab.id === activeId}
              tabIndex={0}
              className={'terminal-tab' + (tab.id === activeId ? ' terminal-tab-active' : '')}
              onClick={() => activateTab(tab.id)}
              onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') activateTab(tab.id); }}
            >
              <TerminalIcon size={12} />
              <span className="terminal-tab-name">{tab.sandboxName}</span>
              {(tab.status === 'exited' || tab.status === 'error') && (
                <span className="terminal-tab-status">{tab.status === 'exited' ? 'exited' : 'lost'}</span>
              )}
              <IconButton
                icon={XIcon}
                variant="invisible"
                size="small"
                className="terminal-tab-close"
                aria-label={`Close ${tab.sandboxName} terminal`}
                onClick={e => { e.stopPropagation(); closeTab(tab.id); }}
              />
            </div>
          ))}
          <ActionMenu>
            <ActionMenu.Anchor>
              <IconButton icon={PlusIcon} variant="invisible" size="small" aria-label="New terminal" />
            </ActionMenu.Anchor>
            <ActionMenu.Overlay>
              <ActionList>
                {capable.length === 0 ? (
                  <ActionList.Item disabled>No terminal-capable sandboxes (ssh, or docker with persistent on)</ActionList.Item>
                ) : (
                  capable.map(s => (
                    <ActionList.Item key={s.id} onSelect={() => addTab(s)}>
                      {s.name}
                    </ActionList.Item>
                  ))
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
                aria-label="Quote selection to chat"
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
          <IconButton icon={XIcon} variant="invisible" size="small" aria-label="Hide terminal panel" onClick={onClose} />
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
              sandboxId={tab.sandboxId}
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
