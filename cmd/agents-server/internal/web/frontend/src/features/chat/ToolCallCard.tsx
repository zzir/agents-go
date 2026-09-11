import { useRef } from 'react';
import { Button, Label } from '@primer/react';
import { StatusLabel } from '@/lib/status';
import { ToolsIcon, StackIcon, SyncIcon, CheckIcon, DotFillIcon, CircleIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { useAsyncMarkdown } from '@/lib/markdown';
import { type ToolCall } from '@/lib/timeline';
import { useChatActions, useChatTaskLookups } from '@/features/chat/ChatSessionContext';
import { ToolOutputBody } from '@/features/chat/ToolOutputBody';
import { WorkflowSpecBody } from '@/features/chat/WorkflowSpecBody';
import { parseWorkflowSpec, type WorkflowSpec } from '@/lib/workflowArgs';

interface ToolCallCardProps {
  toolCall: ToolCall;
  live?: boolean;
  // The task offers — passed by the transcript that anchors the task rather
  // than read from the session, because the same card renders a FOREIGN
  // transcript in the Inspector, where an Inspect would open a task this
  // session does not track. Absent = no offer.
  onInspectTask?: (taskId: string) => void;
  onRetryTask?: (taskId: string) => void;
}

interface TodoRow { content: string; status: string }

type ArgBody =
  | { kind: 'patch' | 'command' | 'json' | 'markdown'; text: string }
  | { kind: 'todos'; text: string; todos: TodoRow[] }
  | { kind: 'workflow'; text: string; spec: WorkflowSpec }
  | { kind: 'memory'; text: string; scope: string; key: string; append: boolean };

// primaryArg picks the meaningful content to show for a tool call. For the tools
// an operator actually reviews at approval time we surface the raw field with
// real newlines instead of an escaped-JSON blob: apply_patch → the patch,
// exec_command → the shell command, submit_plan → the plan as markdown (its
// approval card IS the plan review), todo_write → a checklist, save_workflow →
// the definition (its approval card is the change review). Everything else
// falls back to pretty JSON.
function primaryArg(toolName: string, args: string): ArgBody {
  try {
    const parsed = JSON.parse(args);
    if (toolName === 'apply_patch' && typeof parsed.patch === 'string') {
      return { kind: 'patch', text: parsed.patch };
    }
    if (toolName === 'save_workflow') {
      const spec = parseWorkflowSpec(args);
      if (spec) return { kind: 'workflow', text: '', spec };
    }
    if (toolName === 'exec_command' && typeof parsed.cmd === 'string') {
      const cmd = parsed.workdir ? `cd ${parsed.workdir} && ${parsed.cmd}` : parsed.cmd;
      return { kind: 'command', text: cmd };
    }
    if (toolName === 'submit_plan' && typeof parsed.plan === 'string') {
      return { kind: 'markdown', text: parsed.plan };
    }
    if ((toolName === 'memory_write' || toolName === 'memory_append') && typeof parsed.text === 'string') {
      // The approval card of an agent-scope write IS the review: scope, key,
      // and the text exactly as it would land.
      return { kind: 'memory', text: parsed.text, scope: typeof parsed.scope === 'string' && parsed.scope ? parsed.scope : 'session', key: typeof parsed.key === 'string' ? parsed.key : '', append: toolName === 'memory_append' };
    }
    if (toolName === 'todo_write' && Array.isArray(parsed.todos)) {
      const todos = (parsed.todos as Array<{ content?: string; status?: string }>)
        .filter(td => td && typeof td.content === 'string')
        .map(td => ({ content: td.content as string, status: td.status || 'pending' }));
      return { kind: 'todos', text: '', todos };
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
      case 'todo_write': {
        const todos = Array.isArray(p.todos) ? p.todos : [];
        const done = todos.filter((td: { status?: string }) => td?.status === 'completed').length;
        return todos.length ? { text: `${done}/${todos.length} done`, mono: false } : null;
      }
      case 'submit_plan':
        return typeof p.plan === 'string' && p.plan ? { text: p.plan.split('\n')[0], mono: false } : null;
      case 'save_workflow':
      case 'get_workflow':
        return typeof p.name === 'string' && p.name ? { text: p.name, mono: false } : null;
      case 'memory_write':
      case 'memory_append':
      case 'memory_read':
        return typeof p.key === 'string' && p.key ? { text: (typeof p.scope === 'string' && p.scope ? p.scope + ' · ' : '') + p.key, mono: true } : null;
      case 'memory_search':
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

export function ToolCallCard({ toolCall, live, onInspectTask, onRetryTask }: ToolCallCardProps) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output, task, progress } = toolCall;
  const { approve: onApprove, reject: onReject } = useChatActions();
  const { retryableByCallId, liveTaskStatusByCallId, liveTaskLabelByCallId, taskLabelById } = useChatTaskLookups();
  // Keyed by this session's call ids, so a card in a foreign transcript simply
  // misses — no live badge, no retry offer.
  const taskRetryable = retryableByCallId[tool_call_id];
  const liveTaskStatus = liveTaskStatusByCallId[tool_call_id];
  const liveTaskLabel = liveTaskLabelByCallId[tool_call_id];

  // A spawn_task card is the task's anchor in the timeline: the terminal
  // display projection carries the id on the call side; before that lands, the
  // result's Details bag does (taskResult puts task_id there so no UI has to
  // parse the model-facing text back into fields).
  let inspectTaskId = task?.id || '';
  if (!inspectTaskId && tool_name === 'spawn_task' && typeof toolCall.extra?.task_id === 'string') {
    inspectTaskId = toolCall.extra.task_id;
  }

  const sepIdx = tool_name.indexOf('__');
  const mcpServer = sepIdx > 0 ? tool_name.substring(0, sepIdx) : null;
  // The tool's own heading wins over its name, per the display contract
  // (Title is "a card heading, when the tool name is not it").
  const displayTitle = (toolCall.title || '').trim();
  const displayName = displayTitle || (sepIdx > 0 ? tool_name.substring(sepIdx + 2) : tool_name);

  // The spawn label, shown next to the name so the wide spawn_task card carries
  // real information instead of blank space: terminal from the display
  // projection, pre-terminal from the live run event. Empty for non-task tools.
  const taskTitle = (task?.label || liveTaskLabel || '').trim();
  // The tool's one-line account of what happened ("3 files changed"), from its
  // result's display. Outranks everything inferred from the arguments below:
  // the tool said what it did, so the card need not guess from what was asked.
  const resultSummary = (toolCall.summary || '').trim();

  const body = primaryArg(tool_name, args);
  const patchFileList = body.kind === 'patch' ? patchFiles(body.text) : [];
  const multiFile = patchFileList.length > 1;
  const fileHint = patchFileList.length === 1
    ? patchFileList[0]
    : multiFile
      ? `${patchFileList[0]} +${patchFileList.length - 1}`
      : '';

  // One-line title for the header: the spawn label, else the tool's own result
  // summary, else the patched file(s), else a per-tool arg summary, else the
  // task a task_status/task_stop targets. At most one applies — it makes the
  // collapsed card self-describing.
  let headerSummary: { text: string; mono: boolean } | null = null;
  if (taskTitle) headerSummary = { text: taskTitle, mono: false };
  else if (resultSummary) headerSummary = { text: resultSummary, mono: false };
  else if (fileHint) headerSummary = { text: fileHint, mono: true };
  else if (mcpServer) headerSummary = mcpArgSummary(args);
  else {
    headerSummary = argSummary(tool_name, args);
    if (!headerSummary && (tool_name === 'task_status' || tool_name === 'task_stop')) {
      try {
        const tid = (JSON.parse(args) as { task_id?: string }).task_id;
        const lbl = tid ? taskLabelById[tid] : '';
        if (lbl) headerSummary = { text: lbl, mono: false };
      } catch { /* unparseable args → no title */ }
    }
  }

  // Render the patch as a ```diff block through the shared markdown pipeline, so
  // it uses the same hljs theme (and dark mode) as every other code block.
  const diffText = body.kind === 'patch' ? diffPreview(body.text, multiFile) : '';
  const diffHtml = useAsyncMarkdown(diffText ? '```diff\n' + diffText + '\n```' : '');
  // A submitted plan is markdown by instruction; the approval card is the
  // review surface, so it renders like an answer, not like JSON.
  const planHtml = useAsyncMarkdown(body.kind === 'markdown' ? body.text : '');

  const pendingApproval = !!needs_approval && !status;
  // A call an abandoned pause never ran: the pause was stopped, or a newer
  // message superseded it (invariant 19).
  const notRun = status === 'not_run';
  const isRunning = !!live && !pendingApproval && !notRun && !output && status !== 'completed' && status !== 'rejected';
  // A decision unmounts the buttons; focus moves to the card's header first,
  // so a keyboard user is not dropped on <body>.
  const cardRef = useRef<HTMLDivElement>(null);
  const decide = (send: () => void) => {
    cardRef.current?.querySelector<HTMLElement>('.disclosure-header')?.focus();
    send();
  };
  // A card with live output opens itself: a spinner the user has to click to
  // see through defeats the point of streaming it.

  // A result that reported failure gets a badge even on cards that would
  // otherwise stay quiet. It outranks 'approved' (the outcome over the
  // process) but not 'rejected' — a rejection notice is not the tool failing.
  const failed = !!toolCall.is_error && status !== 'rejected';
  const showStatus = status === 'approved' || status === 'rejected' || pendingApproval || isRunning || failed || notRun;
  const statusLabel = status === 'rejected' ? 'rejected'
    : failed ? 'error'
    : notRun ? 'not run — ' + (toolCall.not_run === 'superseded' ? 'superseded by a newer message' : 'stopped')
    : status === 'approved' ? 'approved'
    : pendingApproval ? 'pending'
    : 'running…';
  const statusVariant = status === 'rejected' ? 'danger'
    : failed ? 'danger'
    : notRun ? 'secondary'
    : status === 'approved' ? 'success'
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
      {task?.status && <StatusLabel status={task.status} prefix="task" />}
      {!task?.status && liveTaskStatus && <StatusLabel status={liveTaskStatus} prefix="task" />}
      {(task?.attempt ?? 0) > 1 && <Label variant="secondary">{'attempt ' + task!.attempt}</Label>}
      {/* Only on a card whose task is FAILED: a retry needs something to
          resume, and the badge above is the card's own word on that. */}
      {task?.status === 'failed' && taskRetryable && inspectTaskId && onRetryTask && (
        <button
          className="ToolCallCard-inspect"
          title="Retry task"
          aria-label="Retry task"
          onClick={e => { e.stopPropagation(); onRetryTask(inspectTaskId); }}
        >
          <SyncIcon size={14} />
        </button>
      )}
      {inspectTaskId && onInspectTask && (
        <button
          className="ToolCallCard-inspect"
          title="Inspect task"
          aria-label="Inspect task"
          onClick={e => { e.stopPropagation(); onInspectTask(inspectTaskId); }}
        >
          <StackIcon size={14} />
        </button>
      )}
      {showStatus && <Label variant={statusVariant}>{statusLabel}</Label>}
    </>
  );

  return (
    <Disclosure
      ref={cardRef}
      icon={ToolsIcon}
      variant="done"
      // A div header: the label nests the inspect/retry buttons of a task
      // card, and a button inside a button is invalid HTML.
      as="div"
      label={headerLabel}
      forceOpen={pendingApproval || (!output && !!progress) || undefined}
      // The checklist IS the information; a collapsed "3/5 done" hides the
      // items the user tracks progress by.
      defaultOpen={body.kind === 'todos'}
      className="ToolCallCard"
    >
      {body.kind === 'patch' ? (
        <div className="ToolCallCard-diff markdown-body" dangerouslySetInnerHTML={{ __html: diffHtml }} />
      ) : body.kind === 'markdown' ? (
        <div className="ToolCallCard-plan markdown-body" dangerouslySetInnerHTML={{ __html: planHtml }} />
      ) : body.kind === 'workflow' ? (
        <WorkflowSpecBody spec={body.spec} pending={pendingApproval} />
      ) : body.kind === 'memory' ? (
        <div className="ToolCallCard-memory">
          <div className="ToolCallCard-memory-head">
            <span className="ToolCallCard-memory-scope">{body.scope} memory</span>
            <span className="ToolCallCard-memory-key">{body.key}</span>
            {body.append && <span className="ToolCallCard-memory-op">append</span>}
          </div>
          <pre className="ToolCallCard-memory-text">{body.text}</pre>
        </div>
      ) : body.kind === 'todos' ? (
        <ul className="ToolCallCard-todos">
          {body.todos.map((td, i) => (
            <li key={i} className={'ToolCallCard-todo ToolCallCard-todo--' + td.status}>
              <span className="ToolCallCard-todo-icon">
                {td.status === 'completed' ? <CheckIcon size={14} />
                  : td.status === 'in_progress' ? <DotFillIcon size={14} />
                  : <CircleIcon size={12} />}
              </span>
              {td.content}
            </li>
          ))}
        </ul>
      ) : (
        <pre>{body.text}</pre>
      )}
      {task?.summary && (
        <div className="ToolCallCard-output">
          <div className="ToolCallCard-output-label">Task result:</div>
          <pre>{task.summary}</pre>
        </div>
      )}
      {/* Live output while the tool runs. It disappears when `output` lands —
          the result replaces it rather than sitting beside it, so the same work
          is never shown twice. */}
      {!output && progress && (
        <div className="ToolCallCard-output ToolCallCard-output--live">
          <div className="ToolCallCard-output-label">Running…</div>
          <pre>{progress}</pre>
        </div>
      )}
      {output && (
        <div className="ToolCallCard-output">
          <div className="ToolCallCard-output-label">Output:</div>
          <ToolOutputBody output={output} />
        </div>
      )}
      {pendingApproval && (
        <div className="ToolCallCard-approval">
          {tool_name === 'exec_command' ? (
            <>
              <Button size="small" variant="primary" onClick={() => decide(() => onApprove && onApprove(tool_call_id, 'once'))}>
                Approve once
              </Button>
              <Button size="small" onClick={() => decide(() => onApprove && onApprove(tool_call_id, 'same'))}>
                Trust this command
              </Button>
              <Button size="small" onClick={() => decide(() => onApprove && onApprove(tool_call_id, 'all'))}>
                Trust all this session
              </Button>
            </>
          ) : (
            <Button size="small" variant="primary" onClick={() => decide(() => onApprove && onApprove(tool_call_id, 'once'))}>
              {tool_name === 'submit_plan' ? 'Approve plan' : tool_name === 'save_workflow' ? 'Save workflow' : 'Approve'}
            </Button>
          )}
          <Button size="small" variant="danger" onClick={() => decide(() => onReject && onReject(tool_call_id))}>
            Reject
          </Button>
        </div>
      )}
    </Disclosure>
  );
}
