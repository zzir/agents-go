import { Button, Label } from '@primer/react';

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
  onApprove?: (id: string) => void;
  onReject?: (id: string) => void;
}

export function ToolCallCard({ toolCall, onApprove, onReject }: ToolCallCardProps) {
  const { tool_call_id, tool_name, arguments: args, needs_approval, status, output } = toolCall;

  const sepIdx = tool_name.indexOf('__');
  const mcpServer = sepIdx > 0 ? tool_name.substring(0, sepIdx) : null;
  const displayName = sepIdx > 0 ? tool_name.substring(sepIdx + 2) : tool_name;

  let parsedArgs = args;
  try { parsedArgs = JSON.stringify(JSON.parse(args), null, 2); } catch (_e) { /* ignore */ }

  const showStatus = status === 'approved' || status === 'rejected' || (needs_approval && !status);
  const statusLabel = status === 'approved' ? 'approved' : status === 'rejected' ? 'rejected' : 'pending';
  const statusVariant = status === 'approved' ? 'success' : status === 'rejected' ? 'danger' : 'accent';

  return (
    <div className="ToolCallCard">
      <div className="ToolCallCard-header">
        <div className="ToolCallCard-meta">
          <span className="ToolCallCard-name">{displayName}</span>
          {mcpServer && <Label variant="secondary">{mcpServer}</Label>}
          {showStatus && <Label variant={statusVariant as any}>{statusLabel}</Label>}
        </div>
      </div>
      <div className="ToolCallCard-body">
        <pre>{parsedArgs}</pre>
        {output && (
          <div className="ToolCallCard-output">
            <div className="ToolCallCard-output-label">Output:</div>
            <pre>{output}</pre>
          </div>
        )}
        {needs_approval && !status && (
          <div className="ToolCallCard-approval">
            <Button size="small" variant="primary" onClick={() => onApprove && onApprove(tool_call_id)}>
              Approve
            </Button>
            <Button size="small" variant="danger" onClick={() => onReject && onReject(tool_call_id)}>
              Reject
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}
