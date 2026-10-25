import { useEffect, useRef, useState } from 'react';
import { Button } from '@primer/react';
import { ChecklistIcon } from '@primer/octicons-react';
import { useChatSession } from '@/features/chat/ChatSessionContext';
import { ChecklistItems } from '@/features/chat/ChecklistItems';

// ChecklistChip floats above the composer while the session has a checklist:
// "Checklist 2/5", opening to the list itself. The cards in the transcript
// are history; this is where the work stands now.
export function ChecklistChip() {
  const { checklist } = useChatSession();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  // The list lies over the transcript, so it closes the way a popover does.
  useEffect(() => {
    if (!open) return;
    const onPress = (e: PointerEvent) => { if (!rootRef.current?.contains(e.target as Node)) setOpen(false); };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('pointerdown', onPress);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onPress);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);
  if (!checklist || checklist.items.length === 0) return null;
  const count = `${checklist.done}/${checklist.items.length}`;
  return (
    <div ref={rootRef} className="checklist-chip">
      {open && (
        <div className="checklist-chip-pop" role="region" aria-label="Current checklist">
          <ChecklistItems items={checklist.items} />
        </div>
      )}
      {/* A narrow column drops the word; the label keeps it. */}
      <Button size="small" className="dock-chip" leadingVisual={ChecklistIcon} aria-label={`Checklist ${count}`} aria-expanded={open} onClick={() => setOpen(v => !v)}>
        <span className="checklist-chip-word">Checklist </span>{count}
      </Button>
    </div>
  );
}
