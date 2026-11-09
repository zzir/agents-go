import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Dialog, Select, Stack, TextInput, useConfirm } from '@primer/react';
import { api } from '@/lib/api';
import { fc } from '@/lib/form';
import { toast } from '@/lib/toast';
import type { EnvVar, Workspace } from '@/lib/binding';
import { EnvEditor, cleanEnv, envError } from '@/components/EnvEditor';
import { WorkspaceEnvDialog } from '@/features/chat/WorkspaceEnvDialog';
import { WorkspaceHostDialog } from '@/features/chat/WorkspaceHostDialog';
import type { WorkspaceMenu } from '@/features/chat/ChatTopBar';

// What the chat does TO a workspace, apart from picking one: creating it, and
// the bound workspace's container — its state, start/stop, export, rebuild,
// and the environment it is created with.

interface SandboxOption {
  id: string;
  name: string;
}

// NewWorkspaceDialog: pick a machine and a template, name the workspace, set its
// environment. The created row is handed back; selecting it is the caller's.
export function NewWorkspaceDialog({ sandboxes, initialSandboxId, onCreated, onClose }: {
  sandboxes: SandboxOption[];
  initialSandboxId: string;
  onCreated: (workspace: Workspace) => void;
  onClose: () => void;
}) {
  const [sandboxId, setSandboxId] = useState(initialSandboxId);
  const [name, setName] = useState('');
  const [env, setEnv] = useState<EnvVar[]>([]);
  const [saving, setSaving] = useState(false);
  const sandbox = sandboxes.find(sb => sb.id === sandboxId);

  const create = async () => {
    if (!sandbox || !name.trim() || saving) return;
    setSaving(true);
    try {
      const created = await api.workspaces.create({ name: name.trim(), sandbox_id: sandbox.id, env: cleanEnv(env) }) as Workspace;
      onCreated(created);
    } catch (e) {
      // 409: a workspace of that name already exists on this machine.
      toast.error((e as Error).message || 'Could not create the workspace');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog
      title="New workspace"
      onClose={onClose}
      width="large"
      footerButtons={[
        { content: 'Cancel', onClick: onClose },
        {
          content: saving ? 'Creating…' : 'Create',
          buttonType: 'primary',
          disabled: !sandbox || !name.trim() || saving || !!envError(env),
          onClick: () => { void create(); },
        },
      ]}
    >
      <Stack gap="normal">
        {fc('Sandbox', (
          <Select block value={sandboxId} onChange={e => setSandboxId(e.target.value)}>
            {sandboxes.map(sb => (
              <Select.Option key={sb.id} value={sb.id}>{sb.name}</Select.Option>
            ))}
          </Select>
        ), 'Where the files live and run; only the image can change later.')}
        {fc('Name', (
          <TextInput
            block
            value={name}
            placeholder="e.g. goagents"
            onChange={e => setName(e.target.value)}
          />
        ), 'Mounted at /workspace; unique per sandbox.')}
        {/* The editor carries its own explanation; a caption here would be a
            third line of small print saying the same. */}
        {fc('Environment', (
          <EnvEditor vars={env} onChange={setEnv} disabled={saving} />
        ), envError(env))}
      </Stack>
    </Dialog>
  );
}

// useWorkspaceMenu is the bound workspace's menu (ChatTopBar.workspaceMenu): the
// container's state and the acts on it. `menu` is null while nothing is
// bound; `dialog` is the environment editor, open or not.
export function useWorkspaceMenu({ workspace, sessionId, rebuildable, hostable, running, onWorkspacesChanged }: {
  workspace: Workspace | null;
  // The session the menu is open in: a rebuild leaves its note there.
  sessionId: string | null;
  // False until the sandbox row declares `rebuild` — never offered on a guess.
  rebuildable: boolean;
  // False until the sandbox row declares `public_host`.
  hostable: boolean;
  running: boolean;
  // The environment dialog closed: the workspace rows may have changed.
  onWorkspacesChanged: () => void;
}): { menu: WorkspaceMenu | null; dialog: ReactNode } {
  const confirmDialog = useConfirm();
  // The workspace whose environment is open for editing, and whether a
  // container call is in flight (both disable the menu).
  const [envWorkspace, setEnvWorkspace] = useState<Workspace | null>(null);
  const [hostWorkspace, setHostWorkspace] = useState<Workspace | null>(null);
  const [containerBusy, setContainerBusy] = useState(false);
  // The compute state, refreshed when the workspace changes and after every act
  // on it. '' means "not asked yet".
  const [sandboxState, setSandboxState] = useState('');
  const [stateLoading, setStateLoading] = useState(false);

  // The state is re-read on the edges that move it; only the newest read lands
  // — invariant 49.
  const stateReqSeq = useRef(0);
  const refreshSandboxState = useCallback(async (workspaceID: string) => {
    const seq = ++stateReqSeq.current;
    setStateLoading(true);
    try {
      const state = (await api.workspaces.sandboxStatus(workspaceID)).state;
      if (seq === stateReqSeq.current) setSandboxState(state);
    } catch {
      // Keep the last known value; '' stays '' and renders Start.
    } finally {
      if (seq === stateReqSeq.current) setStateLoading(false);
    }
  }, []);
  useEffect(() => {
    if (!workspace) { stateReqSeq.current++; setSandboxState(''); return; }
    void refreshSandboxState(workspace.id);
  }, [workspace, refreshSandboxState]);
  useEffect(() => {
    if (workspace) void refreshSandboxState(workspace.id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [running]);

  // Start, stop and rebuild are synchronous and can take an image pull's
  // worth of time, so the menu stays disabled until they answer.
  const startSandbox = async () => {
    if (!workspace || containerBusy) return;
    setContainerBusy(true);
    toast.info('Starting the sandbox…');
    try {
      await api.workspaces.sandboxStart(workspace.id);
      setSandboxState('running');
      toast.success('Sandbox running');
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the sandbox');
    } finally {
      setContainerBusy(false);
    }
  };

  const stopSandbox = async () => {
    if (!workspace || containerBusy) return;
    setContainerBusy(true);
    try {
      const res = await api.workspaces.sandboxStop(workspace.id);
      if (res.stopped) {
        setSandboxState('stopped');
        toast.success('Sandbox stopped — the files are kept');
      } else {
        // A run or an open terminal is still using it; it stops when that ends.
        toast.info('Will stop when the work using it finishes');
      }
    } catch (e) {
      toast.error((e as Error).message || 'Could not stop the sandbox');
    } finally {
      setContainerBusy(false);
    }
  };

  // Export reads the container without changing its lifecycle, so it never
  // takes the containerBusy lock; its own ref only stops a double-click.
  const exportBusy = useRef(false);
  const exportWorkspace = async () => {
    if (!workspace || exportBusy.current) return;
    exportBusy.current = true;
    toast.info('Preparing the archive…');
    try {
      await api.workspaces.exportTar(workspace.id, workspace.name);
    } catch (e) {
      toast.error((e as Error).message || 'Could not export the workspace');
    } finally {
      exportBusy.current = false;
    }
  };

  const rebuildContainer = async () => {
    if (!workspace || containerBusy) return;
    if (!await confirmDialog({
      title: `Rebuild the container for “${workspace.name}”?`,
      content: 'The container is discarded and created again from the image. Files under /workspace survive; anything installed into the container does not, and commands running in it right now will fail. Every session on this workspace shares that container; this session gets a note of the rebuild.',
      confirmButtonType: 'danger',
    })) return;
    setContainerBusy(true);
    toast.info('Rebuilding the container…');
    try {
      await api.workspaces.rebuildContainer(workspace.id, sessionId || undefined);
      setSandboxState('running');
      toast.success('Container rebuilt');
    } catch (e) {
      toast.error((e as Error).message || 'The rebuild failed');
    } finally {
      setContainerBusy(false);
    }
  };

  const menu: WorkspaceMenu | null = workspace ? {
    busy: containerBusy,
    state: sandboxState,
    stateLoading,
    rebuildable,
    hostable,
    onEnv: () => setEnvWorkspace(workspace),
    onHost: () => setHostWorkspace(workspace),
    onStart: () => { void startSandbox(); },
    onStop: () => { void stopSandbox(); },
    onExport: () => { void exportWorkspace(); },
    onRebuild: () => { void rebuildContainer(); },
    onOpen: () => { void refreshSandboxState(workspace.id); },
  } : null;

  const dialog = (
    <>
      {envWorkspace && (
        <WorkspaceEnvDialog
          workspace={envWorkspace}
          sessionCount={envWorkspace.session_count}
          onClose={() => { setEnvWorkspace(null); onWorkspacesChanged(); }}
        />
      )}
      {hostWorkspace && <WorkspaceHostDialog workspace={hostWorkspace} onClose={() => setHostWorkspace(null)} />}
    </>
  );

  return { menu, dialog };
}
