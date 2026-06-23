import React from 'react';
import ReactDOM from 'react-dom/client';
import { ThemeProvider } from '/theme/ThemeProvider.jsx';
import { AppShell } from '/layout/AppShell.jsx';
import { SessionList } from '/features/sessions/SessionList.jsx';
import { ChatView } from '/features/chat/ChatView.jsx';
import { AgentConfigPanel } from '/features/agents/AgentConfigPanel.jsx';
import { McpServerPanel } from '/features/mcp/McpServerPanel.jsx';
import { SkillsPanel } from '/features/skills/SkillsPanel.jsx';
import { MemoryPanel } from '/features/memory/MemoryPanel.jsx';
import { SettingsPanel } from '/features/settings/SettingsPanel.jsx';
import { FileBrowser } from '/features/files/FileBrowser.jsx';
import { FileTree } from '/features/files/FileTree.jsx';
import { FileViewer } from '/features/files/FileViewer.jsx';
import { SandboxPanel } from '/features/sandbox/SandboxPanel.jsx';

const { useState, useCallback, useEffect } = React;
const h = React.createElement;

const DIALOG_TABS = [
  { key: 'agents',   label: 'Agents',   comp: AgentConfigPanel },
  { key: 'sandbox',  label: 'Sandbox',  comp: SandboxPanel },
  { key: 'memory',   label: 'Memory',   comp: MemoryPanel },
  { key: 'mcp',      label: 'MCP',      comp: McpServerPanel },
  { key: 'skills',   label: 'Skills',   comp: SkillsPanel },
  { key: 'general',  label: 'General',  comp: SettingsPanel },
];

function SettingsDialog({ onClose }) {
  const [tab, setTab] = useState('agents');
  const active = DIALOG_TABS.find(t => t.key === tab);

  useEffect(() => {
    document.body.classList.add('dialog-open');
    return () => document.body.classList.remove('dialog-open');
  }, []);

  return h('div', { className: 'dialog-overlay', onClick: (e) => { if (e.target === e.currentTarget) onClose(); } },
    h('div', { className: 'dialog' },
      h('div', { className: 'dialog-header' },
        h('span', { className: 'dialog-title' }, 'Settings'),
        h('button', { className: 'btn btn-invisible btn-sm', onClick: onClose, 'aria-label': 'Close' }, '✕'),
      ),
      h('div', { className: 'dialog-body' },
        h('nav', { className: 'dialog-tabs' },
          DIALOG_TABS.map(t =>
            h('button', {
              key: t.key,
              className: 'dialog-tab' + (tab === t.key ? ' active' : ''),
              onClick: () => setTab(t.key),
            }, t.label),
          ),
        ),
        h('div', { className: 'dialog-content' },
          active ? h(active.comp) : null,
        ),
      ),
    ),
  );
}

function App() {
  const [activeSession, setActiveSession] = useState(null);
  const [selectedFile, setSelectedFile] = useState(null);
  const [view, setView] = useState('chat');
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);

  const sessionPane = h(SessionList, {
    activeId: activeSession,
    onSelect: setActiveSession,
    reloadKey: sessionReloadKey,
  });

  const filePane = h(FileTree, {
    selectedPath: selectedFile,
    onSelect: setSelectedFile,
  });

  const handleSessionUpdated = useCallback(() => {
    setSessionReloadKey(k => k + 1);
  }, []);

  const sidebarPane = view === 'chat'
    ? h('div', { key: 'sidebar-chat', style: { display: 'flex', flexDirection: 'column', height: '100%' } }, sessionPane)
    : view === 'files'
      ? h('div', { key: 'sidebar-files', style: { display: 'flex', flexDirection: 'column', height: '100%' } }, filePane)
      : null;

  let main;
  if (view === 'chat') {
    main = h(ChatView, { sessionId: activeSession, onSessionUpdated: handleSessionUpdated });
  } else if (view === 'files') {
    main = h(FileViewer, { filePath: selectedFile });
  }

  return h(ThemeProvider, null,
    h(AppShell, { view, onViewChange: setView, onSettingsOpen: () => setSettingsOpen(true), sidebarPane }, main),
    settingsOpen && h(SettingsDialog, { onClose: () => setSettingsOpen(false) }),
  );
}

const root = ReactDOM.createRoot(document.getElementById('root'));
root.render(h(App));
