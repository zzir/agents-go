// The one wording every scope flip confirms with (invariant 41): publishing
// and unpublishing alike, from a row's menu, a skills group's heading and the
// admin's management table.
export interface ScopeFlipPrompt {
  title: string;
  content: string;
  confirmButtonContent: string;
  confirmButtonType: 'primary' | 'danger';
}

// owner is the row's author: `id` absent means it has none to return to, and
// `label` is how the caller names them when it can ("you", a directory name).
export function scopeFlipPrompt(name: string, target: 'global' | 'private', owner?: { id?: string; label?: string }): ScopeFlipPrompt {
  if (target === 'global') {
    return {
      title: `Publish “${name}”?`,
      content: 'Every member will see it. Its author keeps it and can still edit it.',
      confirmButtonContent: 'Publish',
      confirmButtonType: 'primary',
    };
  }
  return {
    title: `Unpublish “${name}”?`,
    content: owner?.id
      ? `It returns to ${owner.label || 'its author'} alone; members using it lose access.`
      : 'Members using it lose access. It has no author to return to — transfer it first if someone should keep it.',
    confirmButtonContent: 'Unpublish',
    confirmButtonType: 'danger',
  };
}
