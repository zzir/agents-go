import { useState } from 'react';
import { Button } from '@primer/react';
import { ChecklistIcon } from '@primer/octicons-react';
import { useChatSession } from '@/features/chat/ChatSessionContext';
import { ChecklistItems } from '@/features/chat/ChecklistItems';

// ChecklistChip sits above the composer while the session has a checklist:
// "Checklist 2/5", opening to the list itself. The cards in the transcript
// are history; this is where the work stands now.
export function ChecklistChip() {
  const { checklist } = useChatSession();
  const [open, setOpen] = useState(false);
  if (!checklist || checklist.items.length === 0) return null;
  return (
    <div className="checklist-chip">
      {open && (
        <div className="checklist-chip-pop" role="region" aria-label="Current checklist">
          <ChecklistItems items={checklist.items} />
        </div>
      )}
      <Button size="small" variant="invisible" leadingVisual={ChecklistIcon} aria-expanded={open} onClick={() => setOpen(v => !v)}>
        Checklist {checklist.done}/{checklist.items.length}
      </Button>
    </div>
  );
}
