import { useContext } from 'react';
import { Button, useConfirm } from '@primer/react';
import { useReadOnly } from '@/lib/access';
import { DISCARD_PROMPT, FormDirtyContext } from '@/lib/unsaved';

/** The Save/Cancel/Delete row every settings form ends with. Cancel asks first
 * when the form was edited (UnsavedForm tracks that). Absent in a read-only
 * dialog, where the form is a view closed from its header. */
export function FormActions({ onSave, onCancel, onDelete, size, saving, saveDisabled }: {
  onSave: () => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  size?: 'small';
  // useCrud's in-flight flag: Save waits, so a double click cannot post twice.
  saving?: boolean;
  // The form is not complete yet (a required pick still empty).
  saveDisabled?: boolean;
}) {
  const readOnly = useReadOnly();
  const dirty = useContext(FormDirtyContext);
  const confirm = useConfirm();
  if (readOnly) return null;
  const cancel = async () => {
    if (dirty && !(await confirm(DISCARD_PROMPT))) return;
    onCancel?.();
  };
  return (
    <div className="form-actions">
      <Button onClick={onSave} variant="primary" size={size} loading={saving} disabled={saving || saveDisabled}>Save</Button>
      {onCancel && <Button onClick={() => void cancel()} size={size}>Cancel</Button>}
      {onDelete && <Button onClick={onDelete} variant="danger" size={size} className="form-actions-delete">Delete</Button>}
    </div>
  );
}
