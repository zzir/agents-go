import { useMemo } from 'react';
import { Label } from '@primer/react';
import { renderMarkdown } from '@/lib/markdown';

interface MessageBubbleProps {
  role: string;
  content: string;
}

export function MessageBubble({ role, content }: MessageBubbleProps) {
  const isUser = role === 'user';
  const isSystem = role === 'system';

  const html = useMemo(() => {
    if (isUser || isSystem) return null;
    return renderMarkdown(content);
  }, [content, isUser, isSystem]);

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
}
