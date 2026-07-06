import { Button, Label } from '@primer/react';
import { ToolsIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { useAsyncMarkdown } from '@/lib/markdown';

interface ToolCall {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  needs_approval?: boolean;
  status?: string;
  output?: string;
}

interface ToolCallCardProps {
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

export function ToolCallCard({ toolCall, live, onApprove, onReject }: ToolCallCardProps) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output } = toolCall;

  const sepIdx = tool_name.indexOf('__');
  const mcpServer = sepIdx > 0 ? tool_name.substring(0, sepIdx) : null;
  const displayName = sepIdx > 0 ? tool_name.substring(sepIdx + 2) : tool_name;

  const body = primaryArg(tool_name, args);
  const patchFileList = body.kind === 'patch' ? patchFiles(body.text) : [];
  const multiFile = patchFileList.length > 1;
  const fileHint = patchFileList.length === 1
    ? patchFileList[0]
    : multiFile
      ? `${patchFileList[0]} +${patchFileList.length - 1}`
      : '';

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
      {fileHint && <span className="ToolCallCard-file">{fileHint}</span>}
      {mcpServer && <Label variant="secondary">{mcpServer}</Label>}
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
