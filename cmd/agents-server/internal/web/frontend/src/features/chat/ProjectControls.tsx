import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Dialog, Select, Stack, TextInput, useConfirm } from '@primer/react';
import { api } from '@/lib/api';
import { fc } from '@/lib/form';
import { toast } from '@/lib/toast';
import type { EnvVar, Project } from '@/lib/binding';
import { EnvEditor, cleanEnv, envError } from '@/components/EnvEditor';
import { ProjectEnvDialog } from '@/features/chat/ProjectEnvDialog';
import type { ProjectMenu } from '@/features/chat/ChatTopBar';

// What the chat does TO a project, apart from picking one: creating it, and
// the bound project's container — its state, start/stop, export, rebuild,
// and the environment it is created with.

interface SandboxOption {
  id: string;
  name: string;
}

// NewProjectDialog: pick a machine and a template, name the project, set its
// environment. The created row is handed back; selecting it is the caller's.
export function NewProjectDialog({ sandboxes, initialSandboxId, onCreated, onClose }: {
  sandboxes: SandboxOption[];
  initialSandboxId: string;
  onCreated: (project: Project) => void;
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
      const created = await api.projects.create({ name: name.trim(), sandbox_id: sandbox.id, env: cleanEnv(env) }) as Project;
      onCreated(created);
    } catch (e) {
      // 409: a project of that name already exists on this machine.
      toast.error((e as Error).message || 'Could not create the project');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog
      title="New project"
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
        ), 'The machine the files live on and the image they run in. The machine is fixed once the project exists; the image can change.')}
        {fc('Name', (
          <TextInput
            block
            value={name}
            placeholder="e.g. goagents"
            onChange={e => setName(e.target.value)}
          />
        ), 'Names the working tree the container mounts; unique per sandbox.')}
        {/* The editor carries its own explanation; a caption here would be a
            third line of small print saying the same. */}
        {fc('Environment', (
          <EnvEditor vars={env} onChange={setEnv} disabled={saving} />
        ), envError(env))}
      </Stack>
    </Dialog>
  );
}

// useProjectMenu is the bound project's menu (ChatTopBar.projectMenu): the
// container's state and the acts on it. `menu` is null while nothing is
// bound; `dialog` is the environment editor, open or not.
export function useProjectMenu({ project, rebuildable, running, onProjectsChanged }: {
  project: Project | null;
  // False until the sandbox row declares `rebuild` — never offered on a guess.
  rebuildable: boolean;
  running: boolean;
  // The environment dialog closed: the project rows may have changed.
  onProjectsChanged: () => void;
}): { menu: ProjectMenu | null; dialog: ReactNode } {
  const confirmDialog = useConfirm();
  // The project whose environment is open for editing, and whether a
  // container call is in flight (both disable the menu).
  const [envProject, setEnvProject] = useState<Project | null>(null);
  const [containerBusy, setContainerBusy] = useState(false);
  // The compute state, refreshed when the project changes and after every act
  // on it. '' means "not asked yet".
  const [sandboxState, setSandboxState] = useState('');
  const [stateLoading, setStateLoading] = useState(false);

  // The state is read when the bound project changes, again as the menu opens,
  // and whenever a run starts or ends: a run's first command starts the
  // sandbox without telling this component. A failure leaves the last known
  // value in place (or, on a first read, offers Start — the harmless choice);
  // only the newest read for the current project lands.
  const stateReqSeq = useRef(0);
  const refreshSandboxState = useCallback(async (projectID: string) => {
    const seq = ++stateReqSeq.current;
    setStateLoading(true);
    try {
      const state = (await api.projects.sandboxStatus(projectID)).state;
      if (seq === stateReqSeq.current) setSandboxState(state);
    } catch {
      // Keep the last known value; '' stays '' and renders Start.
    } finally {
      if (seq === stateReqSeq.current) setStateLoading(false);
    }
  }, []);
  useEffect(() => {
    if (!project) { stateReqSeq.current++; setSandboxState(''); return; }
    void refreshSandboxState(project.id);
  }, [project, refreshSandboxState]);
  useEffect(() => {
    if (project) void refreshSandboxState(project.id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [running]);

  // Start, stop and rebuild are synchronous and can take an image pull's
  // worth of time, so the menu stays disabled until they answer.
  const startSandbox = async () => {
    if (!project || containerBusy) return;
    setContainerBusy(true);
    toast.info('Starting the sandbox…');
    try {
      await api.projects.sandboxStart(project.id);
      setSandboxState('running');
      toast.success('Sandbox running');
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the sandbox');
    } finally {
      setContainerBusy(false);
    }
  };

  const stopSandbox = async () => {
    if (!project || containerBusy) return;
    setContainerBusy(true);
    try {
      const res = await api.projects.sandboxStop(project.id);
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
  const exportProject = async () => {
    if (!project || exportBusy.current) return;
    exportBusy.current = true;
    toast.info('Preparing the archive…');
    try {
      await api.projects.exportTar(project.id, project.name);
    } catch (e) {
      toast.error((e as Error).message || 'Could not export the project');
    } finally {
      exportBusy.current = false;
    }
  };

  const rebuildContainer = async () => {
    if (!project || containerBusy) return;
    if (!await confirmDialog({
      title: `Rebuild the container for “${project.name}”?`,
      content: 'The container is discarded and created again from the image. Files under /workspace survive; anything installed into the container does not, and commands running in it right now will fail.',
      confirmButtonType: 'danger',
    })) return;
    setContainerBusy(true);
    toast.info('Rebuilding the container…');
    try {
      await api.projects.rebuildContainer(project.id);
      setSandboxState('running');
      toast.success('Container rebuilt');
    } catch (e) {
      toast.error((e as Error).message || 'The rebuild failed');
    } finally {
      setContainerBusy(false);
    }
  };

  const menu: ProjectMenu | null = project ? {
    busy: containerBusy,
    state: sandboxState,
    stateLoading,
    rebuildable,
    onEnv: () => setEnvProject(project),
    onStart: () => { void startSandbox(); },
    onStop: () => { void stopSandbox(); },
    onExport: () => { void exportProject(); },
    onRebuild: () => { void rebuildContainer(); },
    onOpen: () => { void refreshSandboxState(project.id); },
  } : null;

  const dialog = envProject && (
    <ProjectEnvDialog
      project={envProject}
      sessionCount={envProject.session_count}
      onClose={() => { setEnvProject(null); onProjectsChanged(); }}
    />
  );

  return { menu, dialog };
}
