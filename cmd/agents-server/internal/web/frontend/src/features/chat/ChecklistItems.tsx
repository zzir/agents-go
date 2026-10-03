import { CheckIcon, CircleIcon, DotFillIcon } from '@primer/octicons-react';
import type { ChecklistItem } from '@/lib/checklist';

// ChecklistItems renders a todo_write list: the card body and the chip's
// popover share it.
export function ChecklistItems({ items }: { items: ChecklistItem[] }) {
  return (
    <ul className="ToolCallCard-todos">
      {items.map((td, i) => (
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
  );
}
