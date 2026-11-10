import { memo } from 'react';
import { Label } from '@primer/react';

// A system row that is not a started note: its text as a centered label.
export const SystemMessage = memo(function SystemMessage({ content }: { content: string }) {
  return (
    <div className="message message-system">
      <Label variant="secondary">{content}</Label>
    </div>
  );
});
