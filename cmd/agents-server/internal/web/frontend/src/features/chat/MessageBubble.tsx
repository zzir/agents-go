import { memo } from 'react';
import { Label } from '@primer/react';
import { useAsyncMarkdown } from '@/lib/markdown';

interface MessageBubbleProps {
  role: string;
  content: string;
}

export const MessageBubble = memo(function MessageBubble({ role, content }: MessageBubbleProps) {
  const isUser = role === 'user';
  const isSystem = role === 'system';

  const html = useAsyncMarkdown(isUser || isSystem ? '' : content);

  if (isSystem) {
    return (
      <div className="message message-system">
        <Label variant="secondary">{content}</Label>
      </div>
    );
  }

  if (isUser) {
    return (
      <div className="message message-user">
        <div className="message-body">{content}</div>
      </div>
    );
  }

  return (
    <div className="message message-assistant">
      <div
        className="message-body markdown-body"
        dangerouslySetInnerHTML={{ __html: html ?? '' }}
      />
    </div>
  );
});
