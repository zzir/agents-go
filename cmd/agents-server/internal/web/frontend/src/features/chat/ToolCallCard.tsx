import { Button, Label } from '@primer/react';
import { ToolsIcon, StackIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { useAsyncMarkdown } from '@/lib/markdown';
import { type ToolCall } from '@/lib/timeline';

interface ToolCallCardProps {
  onInspectTask?: (taskId: string) => void;
  // Live status of the task this spawn call started (working/input_required),
  // from run events; terminal status comes from the display projection.
  liveTaskStatus?: string;
  // Live task title (the spawn label) from the run event, so the pre-terminal
  // card carries it too; the terminal title comes from task.label.
  liveTaskLabel?: string;
  // taskId → task label, so task_status / task_stop cards can show the readable
  // title of the task they act on instead of the opaque id.
  taskLabelById?: Record<string, string>;
  toolCall: ToolCall;
  live?: boolean;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
}

type ArgBody = { kind: 'patch' | 'command' | 'json'; text: string };

// primaryArg picks the meaningful content to show for a tool call. For the tools
// an operator actually reviews at approval time we surface the raw field with
// real newlines instead of an escaped-JSON blob: apply_patch → the patch,
// exec_command → the shell command. Everything else falls back to pretty JSON.
function primaryArg(toolName: string, args: string): ArgBody {
  try {
    const parsed = JSON.parse(args);
    if (toolName === 'apply_patch' && typeof parsed.patch === 'string') {
      return { kind: 'patch', text: parsed.patch };
    }
    if (toolName === 'exec_command' && typeof parsed.cmd === 'string') {
      const cmd = parsed.workdir ? `cd ${parsed.workdir} && ${parsed.cmd}` : parsed.cmd;
      return { kind: 'command', text: cmd };
    }
    return { kind: 'json', text: JSON.stringify(parsed, null, 2) };
  } catch {
    return { kind: 'json', text: args };
  }
}

// patchFiles lists the paths a patch touches, for the card header.
function patchFiles(patch: string): string[] {
  const files: string[] = [];
  for (const line of patch.split('\n')) {
    const m = line.match(/^\*\*\* (?:Update|Add|Delete) File: (.+)$/);
    if (m) files.push(m[1].trim());
  }
  return files;
}

// diffPreview strips the patch framing so the body is just the hunks. Begin/End
// carry no info; the single-file name is in the header so its "*** ... File:"
// line goes too. Multi-file patches keep those File lines as group separators.
function diffPreview(patch: string, multiFile: boolean): string {
  return patch.split('\n').filter((line) => {
    if (line.startsWith('*** Begin Patch') || line.startsWith('*** End Patch')) return false;
    if (!multiFile && line.startsWith('*** ')) return false;
    return true;
  }).join('\n');
}

// argSummary picks a one-line title for the card header straight from the call
// args, so a collapsed card is self-describing (the file it reads, the command
// it runs) instead of blank. mono = the code font (paths/commands); sans reads
// as prose (search queries, sub-tool lists). Missing field / unparseable → null
// (no title, never throws). apply_patch and spawn_task carry their title from
// richer sources (patch body / task label) and are resolved in the component.
function argSummary(toolName: string, args: string): { text: string; mono: boolean } | null {
  try {
    const p = JSON.parse(args);
    switch (toolName) {
      case 'exec_command':
        return typeof p.cmd === 'string' && p.cmd ? { text: p.cmd.split('\n')[0], mono: true } : null;
      case 'read_file':
      case 'write_file':
        return typeof p.path === 'string' && p.path ? { text: p.path, mono: true } : null;
      case 'list_files':
        return { text: typeof p.path === 'string' && p.path ? p.path : 'working dir', mono: true };
      case 'brave_search':
        return typeof p.query === 'string' && p.query ? { text: p.query, mono: false } : null;
      case 'multi_tool_use.parallel': {
        const uses = Array.isArray(p.tool_uses) ? p.tool_uses : [];
        const names = uses
          .map((u: { recipient_name?: string; name?: string }) => (typeof u?.recipient_name === 'string' ? u.recipient_name.replace(/^functions\./, '') : u?.name))
          .filter(Boolean);
        return names.length ? { text: names.join(', '), mono: false } : null;
      }
      default:
        return null;
    }
  } catch {
    return null;
  }
}

// mcpArgSummary scans an MCP tool call's args for well-known parameter names
// common across MCP servers (path, query, command, …). Identifiers and paths
// render in the code font (mono); prose queries don't. Returns null when
// nothing useful is found — the card falls back to the bare method name.
function mcpArgSummary(args: string): { text: string; mono: boolean } | null {
  try {
    const p = JSON.parse(args);
    if (typeof p !== 'object' || p === null) return null;
    const mono = ['path', 'uri', 'url', 'file', 'name', 'command', 'cmd', 'label'];
    const sans = ['query', 'q', 'search', 'prompt', 'message'];
    for (const k of mono) {
      if (typeof p[k] === 'string' && p[k]) return { text: p[k], mono: true };
    }
    for (const k of sans) {
      if (typeof p[k] === 'string' && p[k]) return { text: p[k], mono: false };
    }
    return null;
  } catch {
    return null;
  }
}

export function ToolCallCard({ toolCall, live, onApprove, onReject, onInspectTask, liveTaskStatus, liveTaskLabel, taskLabelById }: ToolCallCardProps) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output, task } = toolCall;

  // A spawn_task card is the task's anchor in the timeline: the terminal
  // display projection carries the id; while live, it's in the tool output.
  let inspectTaskId = task?.id || '';
  if (!inspectTaskId && tool_name === 'spawn_task' && output) {
    try { inspectTaskId = (JSON.parse(output) as { task_id?: string }).task_id || ''; } catch { /* not JSON */ }
  }

  const sepIdx = tool_name.indexOf('__');
  const mcpServer = sepIdx > 0 ? tool_name.substring(0, sepIdx) : null;
  const displayName = sepIdx > 0 ? tool_name.substring(sepIdx + 2) : tool_name;

  // The spawn label, shown next to the name so the wide spawn_task card carries
  // real information instead of blank space: terminal from the display
  // projection, pre-terminal from the live run event. Empty for non-task tools.
  const taskTitle = (task?.label || liveTaskLabel || '').trim();

  const body = primaryArg(tool_name, args);
  const patchFileList = body.kind === 'patch' ? patchFiles(body.text) : [];
  const multiFile = patchFileList.length > 1;
  const fileHint = patchFileList.length === 1
    ? patchFileList[0]
    : multiFile
      ? `${patchFileList[0]} +${patchFileList.length - 1}`
      : '';

  // One-line title for the header: the spawn label, else the patched file(s),
  // else a per-tool arg summary, else the task a task_status/task_stop targets.
  // At most one applies — it makes the collapsed card self-describing.
  let headerSummary: { text: string; mono: boolean } | null = null;
  if (taskTitle) headerSummary = { text: taskTitle, mono: false };
  else if (fileHint) headerSummary = { text: fileHint, mono: true };
  else if (mcpServer) headerSummary = mcpArgSummary(args);
  else {
    headerSummary = argSummary(tool_name, args);
    if (!headerSummary && (tool_name === 'task_status' || tool_name === 'task_stop')) {
      try {
        const tid = (JSON.parse(args) as { task_id?: string }).task_id;
        const lbl = tid ? taskLabelById?.[tid] : '';
        if (lbl) headerSummary = { text: lbl, mono: false };
      } catch { /* unparseable args → no title */ }
    }
  }

  // Render the patch as a ```diff block through the shared markdown pipeline, so
  // it uses the same hljs theme (and dark mode) as every other code block.
  const diffText = body.kind === 'patch' ? diffPreview(body.text, multiFile) : '';
  const diffHtml = useAsyncMarkdown(diffText ? '```diff\n' + diffText + '\n```' : '');

  const pendingApproval = !!needs_approval && !status;
  const isRunning = !!live && !pendingApproval && !output && status !== 'completed' && status !== 'rejected';

  const showStatus = status === 'approved' || status === 'rejected' || pendingApproval || isRunning;
  const statusLabel = status === 'approved' ? 'approved'
    : status === 'rejected' ? 'rejected'
    : pendingApproval ? 'pending'
    : 'running…';
  const statusVariant = status === 'approved' ? 'success'
    : status === 'rejected' ? 'danger'
    : pendingApproval ? 'attention'
    : 'accent';

  const headerLabel = (
    <>
      <span className="ToolCallCard-name">{displayName}</span>
      {headerSummary && (
        <span
          className={'ToolCallCard-tasktitle' + (headerSummary.mono ? ' ToolCallCard-tasktitle--mono' : '')}
          title={headerSummary.text}
        >{headerSummary.text}</span>
      )}
      {/* Pushes the trailing group (MCP label / task status / inspect /
          approval status) to the right edge, so every card's header aligns the
          same way whether or not it has a summary. */}
      <span className="ToolCallCard-spacer" />
      {mcpServer && <Label>{mcpServer}</Label>}
      {task?.status && <Label variant={task.status === 'completed' ? 'success' : task.status === 'failed' ? 'danger' : task.status === 'cancelled' ? 'secondary' : 'accent'}>{'task ' + task.status.replace('_', ' ')}</Label>}
      {!task?.status && liveTaskStatus && <Label variant={liveTaskStatus === 'input_required' ? 'attention' : 'accent'}>{'task ' + liveTaskStatus.replace('_', ' ')}</Label>}
      {inspectTaskId && onInspectTask && (
        <button
          className="ToolCallCard-inspect"
          title="Inspect task"
          onClick={e => { e.stopPropagation(); onInspectTask(inspectTaskId); }}
        >
          <StackIcon size={14} />
        </button>
      )}
      {showStatus && <Label variant={statusVariant as any}>{statusLabel}</Label>}
    </>
  );

  return (
    <Disclosure
      icon={ToolsIcon}
      variant="done"
      label={headerLabel}
      forceOpen={pendingApproval || undefined}
      className="ToolCallCard"
    >
      {body.kind === 'patch' ? (
        <div className="ToolCallCard-diff markdown-body" dangerouslySetInnerHTML={{ __html: diffHtml }} />
      ) : (
        <pre>{body.text}</pre>
      )}
      {task?.summary && (
        <div className="ToolCallCard-output">
          <div className="ToolCallCard-output-label">Task result:</div>
          <pre>{task.summary}</pre>
        </div>
      )}
      {output && (
        <div className="ToolCallCard-output">
          <div className="ToolCallCard-output-label">Output:</div>
          <pre>{output}</pre>
        </div>
      )}
      {pendingApproval && (
        <div className="ToolCallCard-approval">
          {tool_name === 'exec_command' ? (
            <>
              <Button size="small" variant="primary" onClick={() => onApprove && onApprove(tool_call_id, 'once')}>
                Approve once
              </Button>
              <Button size="small" onClick={() => onApprove && onApprove(tool_call_id, 'same')}>
                Trust this command
              </Button>
              <Button size="small" onClick={() => onApprove && onApprove(tool_call_id, 'all')}>
                Trust all this session
              </Button>
            </>
          ) : (
            <Button size="small" variant="primary" onClick={() => onApprove && onApprove(tool_call_id, 'once')}>
              Approve
            </Button>
          )}
          <Button size="small" variant="danger" onClick={() => onReject && onReject(tool_call_id)}>
            Reject
          </Button>
        </div>
      )}
    </Disclosure>
  );
}
