import { Button } from '@primer/react';

/** The Save/Cancel/Delete row every settings form ends with. Save's handler
 * keeps the form's own packing/validation; Delete sits alone on the far edge. */
export function FormActions({ onSave, onCancel, onDelete, size }: {
  onSave: () => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  size?: 'small';
}) {
  return (
    <div className="form-actions">
      <Button onClick={onSave} variant="primary" size={size}>Save</Button>
      {onCancel && <Button onClick={onCancel} size={size}>Cancel</Button>}
      {onDelete && <Button onClick={onDelete} variant="danger" size={size} className="form-actions-delete">Delete</Button>}
    </div>
  );
}
