// The settings panels' badge grammar — ONE semantic per color, every panel:
//   secondary  quiet metadata: counts, and a panel's own type axis (the
//              provider-type badge alone carries a per-backend color).
//   accent     a reference to another configured entity, shown by name.
//   done       published or system-provided (scope badge, built-in).
//   success/attention/danger  live status only — and rendered as the dot
//              beside the title (form-status-dot), never a Label.
// Badge ORDER on a row: scope first, then type/reference, counts last.
export const BADGE = {
  /** How many sub-items a row holds, always written "Thing·N" — MCP·2, Steps·3. */
  count: 'secondary',
  /** A panel's own type axis — sandbox kind, MCP transport, OAuth. */
  type: 'secondary',
  /** System-provided, not user-created — e.g. a built-in guardrail. */
  builtin: 'done',
  /** A reference to another configured entity, shown by its name. A row with
   * no reference (a global memory) carries NO badge — the default is silence. */
  ref: 'accent',
  /** A scoped row's visibility: Global, or a foreign Private row in the
   * admin's cross-user view. An own private row carries NO badge. */
  scope: 'done',
} as const;
