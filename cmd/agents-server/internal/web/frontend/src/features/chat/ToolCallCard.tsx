import { Button, Label } from '@primer/react';
import { ToolsIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';

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
  onApprove?: (id: string) => void;
  onReject?: (id: string) => void;
}

export function ToolCallCard({ toolCall, live, onApprove, onReject }: ToolCallCardProps) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output } = toolCall;

  const sepIdx = tool_name.indexOf('__');
  const mcpServer = sepIdx > 0 ? tool_name.substring(0, sepIdx) : null;
  const displayName = sepIdx > 0 ? tool_name.substring(sepIdx + 2) : tool_name;

  let parsedArgs = args;
  try { parsedArgs = JSON.stringify(JSON.parse(args), null, 2); } catch (_e) { /* ignore */ }

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
      <pre>{parsedArgs}</pre>
      {output && (
        <div className="ToolCallCard-output">
          <div className="ToolCallCard-output-label">Output:</div>
          <pre>{output}</pre>
        </div>
      )}
      {pendingApproval && (
        <div className="ToolCallCard-approval">
          <Button size="small" variant="primary" onClick={() => onApprove && onApprove(tool_call_id)}>
            Approve
          </Button>
          <Button size="small" variant="danger" onClick={() => onReject && onReject(tool_call_id)}>
            Reject
          </Button>
        </div>
      )}
    </Disclosure>
  );
}
